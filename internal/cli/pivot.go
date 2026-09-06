package cli

// pivot answers the question a changed requirement forces on a codebase:
// which of the work already done still stands?
//
// A constraint arriving after the code is written ("this dependency may no longer
// touch customer data", "no external network calls from the request path") splits the
// tree into three populations, and the split is what the developer actually needs:
//
//	INVALIDATED  the file itself does the prohibited thing. It cannot stay as written.
//	AT-RISK      the file does nothing prohibited, but it is built on something
//	             invalidated, so it survives only after that foundation is replaced.
//	SAFE         no dependency path reaches an invalidated file. Leave it alone.
//
// The verdicts are produced by rules over graph evidence, never by a model. A file is
// invalidated because a specific IMPORTS or CALLS edge was found in the parsed graph,
// and the edge is printed with the verdict so the reader can check it. Checkpoint
// context, when a checkpoint id is supplied, adds the "why was this built" half that
// the graph cannot know; it enriches the report and can seed additional invalidated
// files, but it is not required for the classifier to run.
//
// This deliberately stays inside the plugin's no-egress contract (`doctor` asserts
// no_egress=true): no model call, no network, no key. Turning a sentence into a
// prohibited symbol is the caller's job — `--dependency` takes it directly, so the
// only interpretive step lives outside this binary and the evidence chain inside it
// stays inspectable.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

// Verdicts. Ordered by severity so a report can sort on them.
const (
	verdictInvalidated = "INVALIDATED"
	verdictAtRisk      = "AT-RISK"
	verdictSafe        = "SAFE"
)

// pivotMaxDepth bounds how far At-Risk propagates from an invalidated file.
//
// Unbounded transitive closure is the failure mode that makes a tool like this
// useless on a real repository: follow every caller of every caller and the whole
// tree turns At-Risk, which tells the reader nothing. The cap matches `impact`'s own
// default depth of 2, and the hop distance is reported per file so a reader can see
// whether a file is a direct dependent or two steps removed.
const pivotMaxDepth = 2

type pivotFlags struct {
	Repo         string
	Dependency   []string
	CheckpointID string
	Format       string
	Worktree     bool
	Profile      string
	CacheDir     string
	DisableCache bool
	Depth        int
}

// pivotEdge is the single piece of evidence behind one verdict: the relation that
// connected this file to something prohibited, in the graph's own terms.
type pivotEdge struct {
	Relation   string  `json:"relation"`
	Target     string  `json:"target"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	// SourceFile is the file at the other end when this is a propagation edge,
	// empty when the edge points at the prohibited dependency itself.
	SourceFile string `json:"source_file,omitempty"`
	Line       int    `json:"line,omitempty"`
}

type pivotFile struct {
	Path    string `json:"path"`
	Verdict string `json:"verdict"`
	// Distance is hops from the nearest invalidated file: 0 for an invalidated
	// file itself, 1 for a direct dependent, and so on up to the depth cap.
	Distance int `json:"distance"`
	Language string `json:"language,omitempty"`
	// Why is a one-line reading of the evidence, in the report's own voice.
	Why string `json:"why"`
	// Evidence is what Why is derived from. Never empty for a non-Safe verdict.
	Evidence []pivotEdge `json:"evidence,omitempty"`
	// InCheckpoint marks a file the supplied checkpoint touched, so the reader can
	// see which of the affected files are ones this session actually wrote.
	InCheckpoint bool `json:"in_checkpoint,omitempty"`
}

type pivotCheckpointFile struct {
	Path            string `json:"path"`
	Status          string `json:"status"`
	ChangedEntities int    `json:"changed_entities"`
	Dependents      int    `json:"dependents"`
}

type pivotResponse struct {
	SchemaVersion string   `json:"schema_version"`
	Provider      string   `json:"provider"`
	Version       string   `json:"version"`
	Repo          string   `json:"repo"`
	Commit        string   `json:"commit,omitempty"`
	Prohibited    []string `json:"prohibited"`
	MaxDepth      int      `json:"max_depth"`

	// Checkpoint, when supplied, is the intent half of the evidence.
	Checkpoint      string                `json:"checkpoint,omitempty"`
	CheckpointBase  string                `json:"checkpoint_base,omitempty"`
	CheckpointHead  string                `json:"checkpoint_head,omitempty"`
	CheckpointFiles []pivotCheckpointFile `json:"checkpoint_files,omitempty"`

	Invalidated []pivotFile `json:"invalidated"`
	AtRisk      []pivotFile `json:"at_risk"`
	Safe        []pivotFile `json:"safe"`

	Counts pivotCounts `json:"counts"`

	// Unreached is the honest half of the answer: files the graph could not
	// analyze for relationships, so their verdict is unknown rather than Safe.
	Unreached       []string               `json:"unreached,omitempty"`
	UnreachedReason string                 `json:"unreached_reason,omitempty"`
	Warnings        []sem.ProviderWarning  `json:"warnings,omitempty"`
	PartialFailures []sem.PartialFailure   `json:"partial_failures"`
	Stats           sem.ProviderStats      `json:"stats"`
	Completeness    sem.CompletenessReport `json:"completeness"`

	IndexCacheHit  bool  `json:"index_cache_hit"`
	IndexLatencyMS int64 `json:"index_latency_ms"`
	QueryLatencyMS int64 `json:"query_latency_ms"`
	TotalLatencyMS int64 `json:"total_latency_ms"`
}

type pivotCounts struct {
	Invalidated int `json:"invalidated"`
	AtRisk      int `json:"at_risk"`
	Safe        int `json:"safe"`
	Unreached   int `json:"unreached"`
	Total       int `json:"total"`
}

func parsePivotFlags(args []string) (pivotFlags, error) {
	flags := pivotFlags{Format: "text", Depth: pivotMaxDepth}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			value, next, err := pivotFlagValue(args, i, "--repo")
			if err != nil {
				return pivotFlags{}, err
			}
			flags.Repo, i = value, next
		case "--dependency":
			value, next, err := pivotFlagValue(args, i, "--dependency")
			if err != nil {
				return pivotFlags{}, err
			}
			// Repeatable, and comma-separated within one value, so a constraint
			// naming several packages does not need several invocations.
			for _, part := range strings.Split(value, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					flags.Dependency = append(flags.Dependency, trimmed)
				}
			}
			i = next
		case "--checkpoint":
			value, next, err := pivotFlagValue(args, i, "--checkpoint")
			if err != nil {
				return pivotFlags{}, err
			}
			flags.CheckpointID, i = value, next
		case "--format":
			value, next, err := pivotFlagValue(args, i, "--format")
			if err != nil {
				return pivotFlags{}, err
			}
			if value != "text" && value != "json" {
				return pivotFlags{}, fmt.Errorf("unknown --format %q (want text or json)", value)
			}
			flags.Format, i = value, next
		case "--depth":
			value, next, err := pivotFlagValue(args, i, "--depth")
			if err != nil {
				return pivotFlags{}, err
			}
			depth, err := parsePositiveInt(value)
			if err != nil || depth < 1 || depth > 5 {
				return pivotFlags{}, fmt.Errorf("--depth wants 1..5, got %q", value)
			}
			flags.Depth, i = depth, next
		case "--profile":
			value, next, err := pivotFlagValue(args, i, "--profile")
			if err != nil {
				return pivotFlags{}, err
			}
			flags.Profile, i = value, next
		case "--cache-dir":
			value, next, err := pivotFlagValue(args, i, "--cache-dir")
			if err != nil {
				return pivotFlags{}, err
			}
			flags.CacheDir, i = value, next
		case "--no-cache":
			flags.DisableCache = true
		case "--worktree":
			flags.Worktree = true
		default:
			return pivotFlags{}, fmt.Errorf("unknown flag %q for pivot", args[i])
		}
	}
	if len(flags.Dependency) == 0 {
		return pivotFlags{}, errors.New("pivot requires at least one --dependency (the prohibited import or package)")
	}
	return flags, nil
}

func pivotFlagValue(args []string, i int, name string) (string, int, error) {
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s requires a value", name)
	}
	return args[i+1], i + 1, nil
}

func parsePositiveInt(value string) (int, error) {
	n := 0
	if value == "" {
		return 0, errors.New("empty")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func runPivot(ctx context.Context, opts Options, args []string) error {
	flags, err := parsePivotFlags(args)
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	profile, err := parseProfile(flags.Profile)
	if err != nil {
		return err
	}

	totalStarted := time.Now()
	indexStarted := totalStarted
	cacheDir := resolveCacheDir(flags.CacheDir, opts.Env.PluginDataDir)
	snapshot, cacheHit, err := sem.LoadOrBuildProviderSnapshot(ctx, repo, opts.Version, sem.ProviderSnapshotOptions{
		NoNetwork:    true,
		Worktree:     flags.Worktree,
		Profile:      profile,
	}, cacheDir, flags.DisableCache)
	if err != nil {
		return err
	}
	indexLatency := time.Since(indexStarted)

	queryStarted := time.Now()
	response := buildPivotResponse(snapshot, flags)
	response.Version = opts.Version
	response.Repo = repo

	// Checkpoint context is optional. When it resolves, it supplies intent the graph
	// has no way to know; when it does not, the classification still stands on graph
	// evidence alone, so a bad id degrades the report rather than failing the run.
	if flags.CheckpointID != "" {
		if err := attachPivotCheckpoint(ctx, repo, flags.CheckpointID, &response); err != nil {
			fmt.Fprintf(opts.Stderr, "pivot: checkpoint %s not attached: %v\n", flags.CheckpointID, err)
		}
	}
	queryLatency := time.Since(queryStarted)

	response.IndexCacheHit = cacheHit
	response.IndexLatencyMS = indexLatency.Milliseconds()
	response.QueryLatencyMS = queryLatency.Milliseconds()
	response.TotalLatencyMS = time.Since(totalStarted).Milliseconds()

	if flags.Format == "json" {
		encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
		encoder.SetEscapeHTML(false)
		return encoder.Encode(response)
	}
	writePivotText(opts.Stdout, response)
	return nil
}

// buildPivotResponse runs the classification. It is a pure function of the snapshot
// and the flags so a test can drive it without a repository on disk.
func buildPivotResponse(snapshot sem.ProviderSnapshot, flags pivotFlags) pivotResponse {
	response := pivotResponse{
		SchemaVersion: snapshot.Header.SchemaVersion,
		Provider:      snapshot.Header.Provider,
		Commit:        snapshot.Header.Commit,
		Prohibited:    flags.Dependency,
		MaxDepth:      flags.Depth,
		Warnings:      snapshot.Header.Warnings,
	}

	symbolFile := make(map[string]string, len(snapshot.Symbols))
	for _, symbol := range snapshot.Symbols {
		symbolFile[symbol.ID] = symbol.FilePath
	}
	fileLanguage := make(map[string]string, len(snapshot.Files))
	for _, file := range snapshot.Files {
		fileLanguage[file.Path] = file.Language
	}

	// endpointFile maps either end of a relation to the file it lives in. A relation
	// can be file->x or symbol->x depending on the relation type, so both shapes
	// resolve through here rather than at each call site.
	endpointFile := func(id string) string {
		if file, ok := symbolFile[id]; ok {
			return file
		}
		if file, ok := fileRecordPath(id); ok {
			return file
		}
		return ""
	}

	verdicts := make(map[string]*pivotFile)
	ensure := func(filePath string) *pivotFile {
		if existing, ok := verdicts[filePath]; ok {
			return existing
		}
		created := &pivotFile{
			Path:     filePath,
			Verdict:  verdictSafe,
			Distance: -1,
			Language: fileLanguage[filePath],
		}
		verdicts[filePath] = created
		return created
	}
	for _, file := range snapshot.Files {
		ensure(file.Path)
	}

	// Rule 1 — direct conflict. A file is invalidated when the graph shows it
	// reaching the prohibited dependency itself, through an import or a call. This
	// is checkpoint-independent on purpose: code written long before anyone enabled
	// Entire still violates a new constraint, and a classifier that could only see
	// checkpointed files would silently clear all of it.
	for _, relation := range snapshot.Relations {
		if relation.Type != "IMPORTS" && relation.Type != "CALLS" && relation.Type != "CONSTRUCTS" {
			continue
		}
		target := prohibitedMatch(relation.ToID, flags.Dependency)
		if target == "" {
			continue
		}
		filePath := endpointFile(relation.FromID)
		if filePath == "" {
			continue
		}
		entry := ensure(filePath)
		entry.Verdict = verdictInvalidated
		entry.Distance = 0
		entry.Evidence = append(entry.Evidence, pivotEdge{
			Relation:   relation.Type,
			Target:     target,
			Confidence: relation.Confidence,
			Reason:     relation.Reason,
			Line:       firstEvidenceLine(relation),
		})
	}
	for _, entry := range verdicts {
		if entry.Verdict == verdictInvalidated {
			entry.Why = fmt.Sprintf("uses %s directly", strings.Join(prohibitedTargets(entry.Evidence), ", "))
		}
	}

	// Rule 2 — propagation. Everything that depends on an invalidated file is
	// At-Risk: its own code breaks no rule, but the ground under it is being
	// replaced. Breadth-first so each file records its true shortest distance, and
	// capped so the answer stays a shortlist rather than the whole repository.
	dependents := buildFileDependents(snapshot, endpointFile)
	frontier := make([]string, 0, 8)
	for filePath, entry := range verdicts {
		if entry.Verdict == verdictInvalidated {
			frontier = append(frontier, filePath)
		}
	}
	sort.Strings(frontier)
	for depth := 1; depth <= flags.Depth && len(frontier) > 0; depth++ {
		next := make([]string, 0, len(frontier))
		for _, invalidatedPath := range frontier {
			edges := dependents[invalidatedPath]
			sort.Slice(edges, func(a, b int) bool { return edges[a].from < edges[b].from })
			for _, edge := range edges {
				entry := ensure(edge.from)
				if entry.Verdict == verdictInvalidated || entry.Distance >= 0 {
					continue
				}
				entry.Verdict = verdictAtRisk
				entry.Distance = depth
				entry.Evidence = append(entry.Evidence, pivotEdge{
					Relation:   edge.relation,
					Target:     invalidatedPath,
					Confidence: edge.confidence,
					Reason:     edge.reason,
					SourceFile: invalidatedPath,
					Line:       edge.line,
				})
				entry.Why = pivotAtRiskWhy(depth, edge.relation, invalidatedPath)
				next = append(next, edge.from)
			}
		}
		sort.Strings(next)
		frontier = next
	}

	// Rule 3 — everything the graph reached with no path to an invalidated file is
	// Safe. Files the graph could only inventory (no relation support for the
	// language) are reported separately: calling them Safe would be asserting an
	// absence the parser never actually checked.
	semanticLanguage := semanticLanguages(snapshot)
	for filePath, entry := range verdicts {
		if entry.Verdict != verdictSafe {
			continue
		}
		if !semanticLanguage[entry.Language] {
			response.Unreached = append(response.Unreached, filePath)
			delete(verdicts, filePath)
			continue
		}
		entry.Why = "no dependency path to any invalidated file"
		entry.Distance = -1
	}
	if len(response.Unreached) > 0 {
		sort.Strings(response.Unreached)
		response.UnreachedReason = "inventory-only language: the graph parses these files for structure but not for call or import relations, so their verdict is unknown rather than Safe"
	}

	for _, entry := range verdicts {
		switch entry.Verdict {
		case verdictInvalidated:
			response.Invalidated = append(response.Invalidated, *entry)
		case verdictAtRisk:
			response.AtRisk = append(response.AtRisk, *entry)
		default:
			response.Safe = append(response.Safe, *entry)
		}
	}
	sortPivotFiles(response.Invalidated)
	sortPivotFiles(response.AtRisk)
	sortPivotFiles(response.Safe)

	response.Counts = pivotCounts{
		Invalidated: len(response.Invalidated),
		AtRisk:      len(response.AtRisk),
		Safe:        len(response.Safe),
		Unreached:   len(response.Unreached),
		Total:       len(response.Invalidated) + len(response.AtRisk) + len(response.Safe) + len(response.Unreached),
	}
	response.PartialFailures = snapshot.Header.PartialFailures
	response.Stats = snapshot.Header.Stats
	response.Completeness = snapshot.Header.Completeness
	return response
}

// fileDependentEdge is one incoming dependency: `from` depends on the file this edge
// is filed under, by way of `relation`.
type fileDependentEdge struct {
	from       string
	relation   string
	confidence float64
	reason     string
	line       int
}

// buildFileDependents inverts the relation list into "who depends on this file",
// which is the direction propagation walks. Self-edges are dropped: a file importing
// its own package tells us nothing about blast radius.
func buildFileDependents(snapshot sem.ProviderSnapshot, endpointFile func(string) string) map[string][]fileDependentEdge {
	dependents := make(map[string][]fileDependentEdge)
	for _, relation := range snapshot.Relations {
		switch relation.Type {
		case "CALLS", "IMPORTS", "CONSTRUCTS", "USES_TYPE", "PARAM_TYPE", "RETURNS_TYPE", "EXTENDS", "IMPLEMENTS":
		default:
			continue
		}
		fromFile := endpointFile(relation.FromID)
		toFile := endpointFile(relation.ToID)
		if fromFile == "" || toFile == "" || fromFile == toFile {
			continue
		}
		dependents[toFile] = append(dependents[toFile], fileDependentEdge{
			from:       fromFile,
			relation:   relation.Type,
			confidence: relation.Confidence,
			reason:     relation.Reason,
			line:       firstEvidenceLine(relation),
		})
	}
	return dependents
}

func pivotAtRiskWhy(depth int, relation, target string) string {
	if depth == 1 {
		return fmt.Sprintf("%s %s directly", strings.ToLower(relation), target)
	}
	return fmt.Sprintf("reaches %s in %d hops via %s", target, depth, strings.ToLower(relation))
}

// prohibitedMatch reports which prohibited name this relation target is, or "" when
// none of them. Matching is on the import path an author would actually write:
// exact, or a path prefix, so "net/http" also catches "net/http/httptest" while
// "net/httpx" is left alone.
func prohibitedMatch(toID string, prohibited []string) string {
	target := toID
	if trimmed, ok := strings.CutPrefix(target, "external:import:"); ok {
		target = trimmed
	} else if trimmed, ok := strings.CutPrefix(target, "external:"); ok {
		target = trimmed
	}
	for _, candidate := range prohibited {
		if target == candidate {
			return candidate
		}
		if strings.HasPrefix(target, candidate+"/") {
			return candidate
		}
		// A symbol id carries its package in the middle, so a call into the
		// prohibited package still resolves when the id is not a bare import.
		if strings.Contains(target, ":"+candidate+":") || strings.Contains(target, "/"+candidate+":") {
			return candidate
		}
	}
	return ""
}

func prohibitedTargets(evidence []pivotEdge) []string {
	seen := make(map[string]bool, len(evidence))
	targets := make([]string, 0, len(evidence))
	for _, edge := range evidence {
		if edge.SourceFile != "" || seen[edge.Target] {
			continue
		}
		seen[edge.Target] = true
		targets = append(targets, edge.Target)
	}
	sort.Strings(targets)
	return targets
}

// fileRecordPath recovers the repository-relative path from a file record id, whose
// shape is "<repo-key>:file:<path>".
func fileRecordPath(id string) (string, bool) {
	marker := ":file:"
	index := strings.Index(id, marker)
	if index < 0 {
		return "", false
	}
	return id[index+len(marker):], true
}

func firstEvidenceLine(relation sem.RelationRecord) int {
	if len(relation.Evidence) == 0 {
		return 0
	}
	return relation.Evidence[0].StartLine
}

func semanticLanguages(snapshot sem.ProviderSnapshot) map[string]bool {
	semantic := make(map[string]bool)
	for language, tier := range snapshot.Header.LanguageTiers {
		if tier == "semantic" {
			semantic[language] = true
		}
	}
	return semantic
}

// sortPivotFiles orders a section the way the reader should work through it: closest
// to the invalidated code first, then alphabetically for a stable report.
func sortPivotFiles(files []pivotFile) {
	sort.Slice(files, func(a, b int) bool {
		if files[a].Distance != files[b].Distance {
			return files[a].Distance < files[b].Distance
		}
		return files[a].Path < files[b].Path
	})
}

// attachPivotCheckpoint folds checkpoint context into a finished classification. The
// checkpoint answers "what did this session build, and how much depends on it" —
// the half of the story the parsed graph cannot see — and marks which affected files
// this session actually wrote.
func attachPivotCheckpoint(ctx context.Context, repo, checkpointID string, response *pivotResponse) error {
	result, err := sem.AnalyzeCheckpoint(ctx, repo, checkpointID)
	if err != nil {
		return err
	}
	response.Checkpoint = checkpointID
	response.CheckpointBase = result.Base
	response.CheckpointHead = result.Head

	touched := make(map[string]bool, len(result.Files))
	for _, file := range result.Files {
		dependents := 0
		for _, change := range file.Changes {
			dependents += change.DependentsCount
		}
		response.CheckpointFiles = append(response.CheckpointFiles, pivotCheckpointFile{
			Path:            file.Path,
			Status:          file.Status,
			ChangedEntities: len(file.Changes),
			Dependents:      dependents,
		})
		touched[file.Path] = true
	}
	mark := func(files []pivotFile) {
		for i := range files {
			if touched[files[i].Path] {
				files[i].InCheckpoint = true
			}
		}
	}
	mark(response.Invalidated)
	mark(response.AtRisk)
	mark(response.Safe)
	return nil
}

// writePivotText renders the report a developer reads. It leads with the verdict
// counts, then the two sections that require action, each line carrying the evidence
// that produced it. Safe files are counted rather than listed: the point of the
// report is the work that is not safe.
func writePivotText(out io.Writer, response pivotResponse) {
	fmt.Fprintf(out, "Pivot: %s\n", strings.Join(response.Prohibited, ", "))
	if response.Commit != "" {
		fmt.Fprintf(out, "Repo %s at %s | depth %d | index %s (%dms)\n",
			path.Base(response.Repo), shortCommit(response.Commit), response.MaxDepth,
			cacheWord(response.IndexCacheHit), response.IndexLatencyMS)
	}
	fmt.Fprintf(out, "%d invalidated, %d at-risk, %d safe",
		response.Counts.Invalidated, response.Counts.AtRisk, response.Counts.Safe)
	if response.Counts.Unreached > 0 {
		fmt.Fprintf(out, ", %d unreached", response.Counts.Unreached)
	}
	fmt.Fprintf(out, " (of %d files)\n", response.Counts.Total)

	if response.Checkpoint != "" {
		fmt.Fprintf(out, "\nCheckpoint %s (%s..%s) touched %d file(s):\n",
			response.Checkpoint, shortCommit(response.CheckpointBase), shortCommit(response.CheckpointHead),
			len(response.CheckpointFiles))
		for _, file := range response.CheckpointFiles {
			fmt.Fprintf(out, "- %s [%s] %d changed entities, %d dependents\n",
				file.Path, file.Status, file.ChangedEntities, file.Dependents)
		}
	}

	if len(response.Invalidated) == 0 {
		fmt.Fprintf(out, "\nNothing invalidated: no file reaches %s in the graph.\n", strings.Join(response.Prohibited, " or "))
		fmt.Fprintf(out, "Either the constraint does not bite here, or the dependency is named differently in this repo.\n")
	} else {
		fmt.Fprintf(out, "\nINVALIDATED (%d) — breaks the constraint, must be replaced:\n", len(response.Invalidated))
		for _, file := range response.Invalidated {
			writePivotFileLine(out, file)
		}
	}

	if len(response.AtRisk) > 0 {
		fmt.Fprintf(out, "\nAT-RISK (%d) — depends on invalidated code, rework and test in this order:\n", len(response.AtRisk))
		for _, file := range response.AtRisk {
			writePivotFileLine(out, file)
		}
	}

	if len(response.Safe) > 0 {
		fmt.Fprintf(out, "\nSAFE (%d) — no dependency path to invalidated code, leave alone.\n", len(response.Safe))
	}
	if len(response.Unreached) > 0 {
		fmt.Fprintf(out, "\nUNREACHED (%d) — %s\n", len(response.Unreached), response.UnreachedReason)
	}

	// Static analysis reads source without running it, so a call made through an
	// interface or reflection leaves no edge to follow. Saying so is not a
	// disclaimer: a reader who trusts a Safe verdict absolutely will eventually
	// ship a break this tool could never have seen.
	fmt.Fprintf(out, "\nVerify before acting: Safe means no path was found, not that none exists.\n")
	fmt.Fprintf(out, "Calls through interfaces, reflection, or generated code leave no edge to follow.\n")
}

func writePivotFileLine(out io.Writer, file pivotFile) {
	marker := ""
	if file.InCheckpoint {
		marker = " *"
	}
	fmt.Fprintf(out, "- %s%s\n", file.Path, marker)
	fmt.Fprintf(out, "    %s\n", file.Why)
	for _, edge := range file.Evidence {
		location := ""
		if edge.Line > 0 {
			location = fmt.Sprintf(":%d", edge.Line)
		}
		fmt.Fprintf(out, "    evidence: %s -> %s%s (confidence %.2f) %s\n",
			edge.Relation, edge.Target, location, edge.Confidence, edge.Reason)
	}
}

func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func cacheWord(hit bool) string {
	if hit {
		return "cache-hit"
	}
	return "cache-miss"
}
