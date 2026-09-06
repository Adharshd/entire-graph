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
	// verdictUnverified is Safe's honest twin: no path to an invalidated file was
	// found, but this file did not parse cleanly, so the absence of a path is not
	// a finding about the code. Splitting it out is the point -- Safe used to
	// absorb both, and a reader could not tell which one they were holding.
	verdictUnverified = "UNVERIFIED"
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
	ExcludeTests bool
	// RepoRoot is set by runPivot after the repo resolves. It exists only so the
	// GENERATED role can be decided by the file's own header, which the snapshot
	// does not carry. Left empty, classification does no disk I/O at all, which is
	// what keeps buildPivotResponse drivable from a test with no repository.
	RepoRoot string
}

// pivotEdge is the single piece of evidence behind one verdict: the relation that
// connected this file to something prohibited, in the graph's own terms.
type pivotEdge struct {
	Relation   string  `json:"relation"`
	Target     string  `json:"target"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	// Resolution is the graph's own word for how it found this edge -- "exact"
	// when it followed a call to a definition, "name_only" when it matched a
	// globally unique method name. It was previously dropped on the floor, which
	// is what made a resolved edge and a guessed one indistinguishable.
	Resolution string `json:"resolution,omitempty"`
	// Tier grades this edge CONFIRMED, HEURISTIC or UNVERIFIED. It is derived
	// from Resolution, Confidence and whether the file parsed cleanly, so a
	// reader does not have to know the resolution vocabulary to know what to
	// trust. See pivot_evidence.go.
	Tier string `json:"evidence_tier"`
	// SourceFile is the file at the other end when this is a propagation edge,
	// empty when the edge points at the prohibited dependency itself.
	SourceFile string `json:"source_file,omitempty"`
	// Line is a line in the file this evidence is attached to -- the import or call
	// site where that file reaches out -- NOT a line in Target or SourceFile. Pairing
	// it with the other end is what produced citations like "regexp.go:676" for a
	// file of 413 lines, where 676 was the call site in the caller.
	Line int `json:"line,omitempty"`
}

type pivotFile struct {
	Path    string `json:"path"`
	Verdict string `json:"verdict"`
	// EvidenceQuality is the weakest tier among this file's evidence, because a
	// verdict is only as good as the shakiest edge holding it up. Empty when the
	// file has no evidence at all, which is what Safe means.
	EvidenceQuality string `json:"evidence_quality,omitempty"`
	// VerificationRequired says this verdict must not be acted on from the graph
	// alone. It is the safe fallback the report owes a reader whenever the
	// evidence is heuristic or the parse was incomplete.
	VerificationRequired bool `json:"verification_required"`
	// AnalysisNote carries the parser's own reason when this file did not parse
	// cleanly, so the caveat travels attached to the file it applies to rather
	// than sitting in a header the reader has to correlate by hand.
	AnalysisNote string `json:"analysis_note,omitempty"`
	// Distance is hops from the nearest invalidated file: 0 for an invalidated
	// file itself, 1 for a direct dependent, and so on up to the depth cap.
	Distance int    `json:"distance"`
	Language string `json:"language,omitempty"`
	// Role is what kind of file this is (production, test, fixture, generated,
	// vendored). It changes how a verdict should be read, not the verdict itself:
	// a hundred at-risk test files in one package are one command to run, while
	// one at-risk production file is a change someone has to make by hand.
	Role string `json:"role"`
	// RootInvalidated is the invalidated file this one traces back to. At two hops
	// the evidence edge names the intermediate file, which is the wrong thing to
	// order work by: what a reader needs is the root that has to be fixed first.
	RootInvalidated string `json:"root_invalidated,omitempty"`
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
	ExcludeTests  bool     `json:"exclude_tests,omitempty"`

	// Checkpoint, when supplied, is the intent half of the evidence.
	Checkpoint      string                `json:"checkpoint,omitempty"`
	CheckpointBase  string                `json:"checkpoint_base,omitempty"`
	CheckpointHead  string                `json:"checkpoint_head,omitempty"`
	CheckpointFiles []pivotCheckpointFile `json:"checkpoint_files,omitempty"`

	Invalidated []pivotFile `json:"invalidated"`
	AtRisk      []pivotFile `json:"at_risk"`
	Safe        []pivotFile `json:"safe"`

	Counts pivotCounts `json:"counts"`
	// RoleCounts is the whole tree broken down by file role, so a consumer can see
	// the shape of the repository the verdicts were drawn from.
	RoleCounts map[string]int `json:"role_counts,omitempty"`

	// Unreached is the honest half of the answer: files the graph could not
	// analyze for relationships, so their verdict is unknown rather than Safe.
	Unreached       []string `json:"unreached,omitempty"`
	UnreachedReason string   `json:"unreached_reason,omitempty"`
	// Unverified holds files that reached no invalidated file AND did not parse
	// cleanly. They were previously reported Safe.
	Unverified       []pivotFile `json:"unverified"`
	UnverifiedReason string      `json:"unverified_reason,omitempty"`
	// EvidenceCounts totals the classified files by evidence tier, so a reader
	// sees how much of the report rests on inference before reading any of it.
	EvidenceCounts map[string]int `json:"evidence_counts,omitempty"`

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
	// Unverified is Safe minus the certainty: no path found, in a file the parser
	// could not fully read.
	Unverified int `json:"unverified"`
	Unreached  int `json:"unreached"`
	Total      int `json:"total"`
	// ExcludedTests is how many files --exclude-tests removed from the run. It is
	// reported rather than silently dropped: a file that was never classified is
	// not a file that came back Safe.
	ExcludedTests int `json:"excluded_tests,omitempty"`
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
			if value != "text" && value != "json" && value != "plan" {
				return pivotFlags{}, fmt.Errorf("unknown --format %q (want text, json or plan)", value)
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
		case "--exclude-tests":
			flags.ExcludeTests = true
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

	flags.RepoRoot = repo

	totalStarted := time.Now()
	indexStarted := totalStarted
	cacheDir := resolveCacheDir(flags.CacheDir, opts.Env.PluginDataDir)
	snapshot, cacheHit, err := sem.LoadOrBuildProviderSnapshot(ctx, repo, opts.Version, sem.ProviderSnapshotOptions{
		NoNetwork: true,
		Worktree:  flags.Worktree,
		Profile:   profile,
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

	if flags.Format == "json" || flags.Format == "plan" {
		encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
		encoder.SetEscapeHTML(false)
		// json is the classification in full; plan is the work order derived from
		// it. Both are emitted from the same finished response, so a plan can never
		// disagree with the report it came from.
		if flags.Format == "plan" {
			encoder.SetIndent("", "  ")
			return encoder.Encode(buildPivotPlan(response, flags))
		}
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
		ExcludeTests:  flags.ExcludeTests,
		Warnings:      snapshot.Header.Warnings,
	}

	// The parser's own diagnostics, keyed by the file they were reported against.
	// buildPivotResponse used to copy these into the response and never read them,
	// so a file whose parse had failed could still be reported Safe. They are now
	// consulted at the two places it matters: grading an edge, and deciding
	// whether "no path found" is a finding or an absence of information.
	incompleteFiles := incompleteAnalysisFiles(snapshot.Header)

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

	// classified decides whether a file takes part in the run at all. --exclude-tests
	// drops test files before classification rather than filtering them out of the
	// finished report, which matters for propagation as much as for the listing: a
	// test file is not a foundation anything ships on, so a production file should
	// not be pulled in At-Risk by a path that only runs through one.
	classified := func(filePath string) bool {
		return !flags.ExcludeTests || !isConventionalTestPath(filePath)
	}
	excludedTests := 0

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
		if !classified(file.Path) {
			excludedTests++
			continue
		}
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
		if filePath == "" || !classified(filePath) {
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
			Resolution: relation.Resolution,
			Tier: classifyEvidenceTier(relation.Type, relation.Resolution, relation.Confidence,
				relation.WarningCodes, filePath, incompleteFiles),
			Line: firstEvidenceLine(relation),
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
	// rootOf carries the originating invalidated file along the walk. Without it a
	// two-hop file would report its intermediate neighbour as the thing to fix.
	rootOf := make(map[string]string)
	for filePath, entry := range verdicts {
		if entry.Verdict == verdictInvalidated {
			frontier = append(frontier, filePath)
			rootOf[filePath] = filePath
			entry.RootInvalidated = filePath
		}
	}
	sort.Strings(frontier)
	for depth := 1; depth <= flags.Depth && len(frontier) > 0; depth++ {
		next := make([]string, 0, len(frontier))
		for _, invalidatedPath := range frontier {
			edges := dependents[invalidatedPath]
			sort.Slice(edges, func(a, b int) bool { return edges[a].from < edges[b].from })
			for _, edge := range edges {
				if !classified(edge.from) {
					continue
				}
				entry := ensure(edge.from)
				if entry.Verdict == verdictInvalidated || entry.Distance >= 0 {
					continue
				}
				entry.Verdict = verdictAtRisk
				entry.Distance = depth
				entry.RootInvalidated = rootOf[invalidatedPath]
				rootOf[edge.from] = entry.RootInvalidated
				entry.Evidence = append(entry.Evidence, pivotEdge{
					Relation:   edge.relation,
					Target:     invalidatedPath,
					Confidence: edge.confidence,
					Reason:     edge.reason,
					Resolution: edge.resolution,
					Tier: classifyEvidenceTier(edge.relation, edge.resolution, edge.confidence,
						edge.warningCodes, edge.from, incompleteFiles),
					SourceFile: invalidatedPath,
					Line:       edge.line,
				})
				entry.Why = pivotAtRiskWhy(depth, edge.relation, invalidatedPath, entry.RootInvalidated)
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
		if code, incomplete := incompleteFiles[filePath]; incomplete {
			// The distinction this whole change exists for. "No path found" in a
			// file the parser fully read is a finding about the code. The same
			// sentence about a file it could not read is a finding about the
			// parser, and calling it Safe presents the second as the first.
			entry.Verdict = verdictUnverified
			entry.Why = "no dependency path found, but this file did not parse cleanly (" + code + "), so no path found is not the same as no path"
			entry.AnalysisNote = code
			entry.Distance = -1
			continue
		}
		entry.Why = "no dependency path to any invalidated file"
		entry.Distance = -1
	}
	if len(response.Unreached) > 0 {
		sort.Strings(response.Unreached)
		response.UnreachedReason = "inventory-only language: the graph parses these files for structure but not for call or import relations, so their verdict is unknown rather than Safe"
	}

	// Evidence quality is graded once the verdicts are final, and like roles it
	// never changes one. It answers a different question: not "what is this file"
	// but "how well does the graph actually know that". A CONFIRMED verdict can be
	// acted on from the report; anything else has to be checked against source.
	response.EvidenceCounts = make(map[string]int, 3)
	for _, entry := range verdicts {
		entry.EvidenceQuality = weakestEvidenceTier(entry.Evidence)
		if entry.Verdict == verdictUnverified {
			entry.EvidenceQuality = evidenceUnverified
		}
		switch entry.EvidenceQuality {
		case "":
			// Safe with no evidence is not weak evidence. The verdict is the
			// absence of a path, and the report already says so.
		case evidenceConfirmed:
			response.EvidenceCounts[evidenceConfirmed]++
		default:
			response.EvidenceCounts[entry.EvidenceQuality]++
			entry.VerificationRequired = true
		}
	}

	// Roles are decided once, after the verdicts are final: a role never changes a
	// verdict, it only decides how the finished report groups and ranks it.
	response.RoleCounts = make(map[string]int, 6)
	for _, entry := range verdicts {
		entry.Role = classifyPivotRole(entry.Path, flags.RepoRoot)
		response.RoleCounts[entry.Role]++
	}
	for _, filePath := range response.Unreached {
		response.RoleCounts[classifyPivotRole(filePath, flags.RepoRoot)]++
	}

	for _, entry := range verdicts {
		switch entry.Verdict {
		case verdictInvalidated:
			response.Invalidated = append(response.Invalidated, *entry)
		case verdictAtRisk:
			response.AtRisk = append(response.AtRisk, *entry)
		case verdictUnverified:
			response.Unverified = append(response.Unverified, *entry)
		default:
			response.Safe = append(response.Safe, *entry)
		}
	}
	sortPivotFiles(response.Invalidated)
	sortPivotFiles(response.AtRisk)
	sortPivotFiles(response.Safe)
	sortPivotFiles(response.Unverified)
	if len(response.Unverified) > 0 {
		response.UnverifiedReason = "the parser reported a warning or partial failure against these files, so the absence of a dependency path is unknown rather than established; verify against source or a test before treating one as Safe"
	}

	response.Counts = pivotCounts{
		Invalidated:   len(response.Invalidated),
		AtRisk:        len(response.AtRisk),
		Safe:          len(response.Safe),
		Unverified:    len(response.Unverified),
		Unreached:     len(response.Unreached),
		Total:         len(response.Invalidated) + len(response.AtRisk) + len(response.Safe) + len(response.Unverified) + len(response.Unreached),
		ExcludedTests: excludedTests,
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
	// resolution and warningCodes travel with the edge so propagation can be
	// graded the same way a direct conflict is. Without them an At-Risk verdict
	// two hops out would carry no indication of how it was reached.
	resolution   string
	warningCodes []string
	line         int
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
			from:         fromFile,
			relation:     relation.Type,
			confidence:   relation.Confidence,
			resolution:   relation.Resolution,
			warningCodes: relation.WarningCodes,
			reason:       relation.Reason,
			line:         firstEvidenceLine(relation),
		})
	}
	return dependents
}

func pivotAtRiskWhy(depth int, relation, target, root string) string {
	if depth == 1 {
		return fmt.Sprintf("%s %s directly", strings.ToLower(relation), target)
	}
	// Name the root as well as the neighbour: the neighbour is how it was reached,
	// the root is what has to be replaced before this file can be trusted again.
	return fmt.Sprintf("%s %s in %d hops, rooted at %s", strings.ToLower(relation), target, depth, root)
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
		// An internal package is named by a path, not by an import target, and
		// its id embeds the defining file rather than ending at the package:
		//
		//	local/entire-graph:file:internal/sem/analyze.go
		//	local/entire-graph:Go:internal/sem/provider.go:function:StreamSnapshot
		//
		// Neither shape matches any rule above, so an architecture constraint
		// naming an internal package ("nothing outside the API layer may touch
		// internal/sem") returned zero invalidated files. Zero reads as
		// compliance; it meant the question was never understood -- the same
		// class of mistake as reporting an unparsed file SAFE.
		if strings.Contains(target, ":"+candidate+"/") || strings.Contains(target, "/"+candidate+"/") {
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
