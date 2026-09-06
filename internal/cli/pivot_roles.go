package cli

// File roles exist to fix a specific failure in the first version of the report:
// at-risk noise. A constraint that bites one shared test helper drags every test
// in the package along with it, and the report dutifully printed sixty-four file
// names with sixty-four evidence lines. All of that is true and none of it is
// actionable — the reader's actual next move is one `go test ./internal/sem`.
//
// So the role never changes a verdict. It changes how the finished report is
// grouped and ranked: production work is listed file by file with its evidence,
// because someone has to open each one; test fallout is collapsed to a line per
// package with the command that checks it. Nothing is dropped — the JSON carries
// every file with its role, and the collapsed lines carry their own counts.

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Roles a file can hold. Decided by deterministic signals — a path shape or a
// header the generator itself wrote — never by inference.
const (
	roleProduction  = "PRODUCTION"
	roleTest        = "TEST"
	roleTestFixture = "TEST_FIXTURE"
	roleGenerated   = "GENERATED"
	roleVendor      = "VENDOR"
	roleUnknown     = "UNKNOWN"
)

// generatedHeaderPattern is the convention a generator stamps on its own output.
// Go's is fixed by tooling ("^// Code generated .* DO NOT EDIT\\.$"); the other
// comment leaders cover the same convention as it appears in the other languages
// this graph parses.
var generatedHeaderPattern = regexp.MustCompile(`(?m)^[ \t]*(?://|#|--|/\*|\*)[ \t]*Code generated .* DO NOT EDIT\.`)

// generatedHeaderScanBytes bounds the probe. The convention places the line above
// the package or module declaration, so a header that is not in the first few
// kilobytes is not a header a generator wrote.
const generatedHeaderScanBytes = 4096

// classifyPivotRole decides a file's role from signals that cannot be argued with.
//
// Order matters where signals overlap, and each precedence is a judgement about
// which fact dominates:
//
//	vendor/    wins over everything — vendored code is not this repository's to edit.
//	testdata/  wins over the _test.go suffix, because a _test.go file inside
//	           testdata/ is fixture input that the toolchain never builds.
//	generated  is checked only for files that would otherwise be production, so
//	           the role says the useful thing: regenerate this, do not hand-edit it.
//
// repoRoot empty means the generated probe is skipped entirely and no file is read
// from disk, which is what lets the classifier run inside a unit test that has no
// repository behind it.
func classifyPivotRole(filePath, repoRoot string) string {
	segments := strings.Split(filePath, "/")
	directories := segments
	if len(segments) > 0 {
		directories = segments[:len(segments)-1]
	}
	for _, segment := range directories {
		if segment == "vendor" {
			return roleVendor
		}
	}
	for _, segment := range directories {
		switch segment {
		case "testdata", "fixtures", "mocks":
			return roleTestFixture
		}
	}
	base := path.Base(filePath)
	if strings.HasSuffix(base, "_test.go") {
		return roleTest
	}
	// The other languages' test conventions, kept in step with the same predicate
	// --exclude-tests uses, so a file cannot be excluded as a test by one rule and
	// reported as production by the other.
	if isConventionalTestPath(filePath) {
		return roleTest
	}
	if repoRoot == "" {
		return roleProduction
	}
	generated, readable := hasGeneratedHeader(filepath.Join(repoRoot, filepath.FromSlash(filePath)))
	if !readable {
		// The snapshot lists a file the working tree does not have, so the last
		// signal could not be checked. Say unknown rather than assume production.
		return roleUnknown
	}
	if generated {
		return roleGenerated
	}
	return roleProduction
}

func hasGeneratedHeader(absolutePath string) (generated bool, readable bool) {
	file, err := os.Open(absolutePath)
	if err != nil {
		return false, false
	}
	defer file.Close()
	buffer := make([]byte, generatedHeaderScanBytes)
	n, err := file.Read(buffer)
	if n == 0 && err != nil {
		// An empty file is readable and simply carries no header.
		if info, statErr := file.Stat(); statErr == nil && info.Size() == 0 {
			return false, true
		}
		return false, false
	}
	return generatedHeaderPattern.Match(buffer[:n]), true
}

// isTestRole is the split the report groups on: work someone opens a file to do,
// versus work a test command covers in one go.
func isTestRole(role string) bool {
	return role == roleTest || role == roleTestFixture
}

// roleRank orders files inside a section so the reader meets shipped code first.
func roleRank(role string) int {
	switch role {
	case roleProduction:
		return 0
	case roleGenerated:
		return 1
	case roleUnknown:
		return 2
	case roleVendor:
		return 3
	case roleTest:
		return 4
	case roleTestFixture:
		return 5
	}
	return 6
}

// pivotPackageGroup is test fallout collapsed to the unit a reader acts on.
type pivotPackageGroup struct {
	Package string `json:"package"`
	Files   int    `json:"files"`
	// Command is the narrowest check that covers the whole group.
	Command string `json:"command"`
	// Nearest is the smallest hop distance in the group, so a package holding a
	// direct dependent sorts above one that is only reached in two hops.
	Nearest int `json:"nearest_distance"`
}

// collapsePivotPackages turns a list of files into one line per package. This is
// the whole point of the role pass: sixty-four files that all call one shared test
// helper are one command, and printing them individually buries the production
// work that actually needs a person.
func collapsePivotPackages(files []pivotFile) []pivotPackageGroup {
	type accumulator struct {
		files   int
		nearest int
		goFile  bool
	}
	byPackage := make(map[string]*accumulator)
	for _, file := range files {
		key := pivotPackageOf(file.Path)
		entry, ok := byPackage[key]
		if !ok {
			entry = &accumulator{nearest: file.Distance}
			byPackage[key] = entry
		}
		entry.files++
		if file.Distance >= 0 && (entry.nearest < 0 || file.Distance < entry.nearest) {
			entry.nearest = file.Distance
		}
		if file.Language == "go" || strings.HasSuffix(file.Path, ".go") {
			entry.goFile = true
		}
	}
	groups := make([]pivotPackageGroup, 0, len(byPackage))
	for name, entry := range byPackage {
		groups = append(groups, pivotPackageGroup{
			Package: name,
			Files:   entry.files,
			Command: pivotPackageCommand(name, entry.goFile),
			Nearest: entry.nearest,
		})
	}
	sort.Slice(groups, func(a, b int) bool {
		if groups[a].Files != groups[b].Files {
			return groups[a].Files > groups[b].Files
		}
		return groups[a].Package < groups[b].Package
	})
	return groups
}

func pivotPackageOf(filePath string) string {
	directory := path.Dir(filePath)
	if directory == "" || directory == "." {
		return "."
	}
	return directory
}

// pivotPackageCommand names the narrowest check that covers a package. A package
// with no Go in it gets an honest instruction rather than a command that would
// fail: the graph knows the files, not the other toolchains' runners.
func pivotPackageCommand(pkg string, hasGo bool) string {
	if !hasGo {
		return "review " + pkg + "/ (no Go test target derivable)"
	}
	if pkg == "." {
		return "go test ."
	}
	return "go test ./" + pkg
}

// writePivotText renders the report a developer reads: a ranked summary of the four
// kinds of work the constraint creates, then each kind in the order it should be
// worked through. Production sections list every file with its evidence, because
// each is a change someone makes by hand. Test fallout collapses to a command per
// package. Safe files are counted, not listed — the report is about what is not safe.
func writePivotText(out io.Writer, response pivotResponse) {
	fmt.Fprintf(out, "Pivot: %s\n", strings.Join(response.Prohibited, ", "))
	// Depth and index provenance belong to every run, not only committed ones: they are
	// how a reader knows how far propagation was allowed to walk and whether the verdict
	// came off a warm cache. A worktree run has no committed head, so name that state
	// rather than dropping the line and the two facts travelling with it.
	revision := "uncommitted worktree"
	if response.Commit != "" {
		revision = "at " + shortCommit(response.Commit)
	}
	fmt.Fprintf(out, "Repo %s %s | depth %d | index %s (%dms)\n",
		pivotRepoName(response.Repo), revision, response.MaxDepth,
		cacheWord(response.IndexCacheHit), response.IndexLatencyMS)
	fmt.Fprintf(out, "%d invalidated, %d at-risk, %d safe",
		response.Counts.Invalidated, response.Counts.AtRisk, response.Counts.Safe)
	if response.Counts.Unverified > 0 {
		fmt.Fprintf(out, ", %d unverified", response.Counts.Unverified)
	}
	if response.Counts.Unreached > 0 {
		fmt.Fprintf(out, ", %d unreached", response.Counts.Unreached)
	}
	fmt.Fprintf(out, " (of %d files)\n", response.Counts.Total)
	// How much of this report rests on inference, said before any of it is read.
	// A reader deciding how far to trust a report does that first, not after.
	if mix := pivotEvidenceMix(response.EvidenceCounts); mix != "" {
		fmt.Fprintf(out, "Evidence: %s\n", mix)
	}
	if response.ExcludeTests {
		// Said out loud rather than left as a silently shorter list: an excluded
		// file was never classified, which is not the same as coming back Safe.
		fmt.Fprintf(out, "--exclude-tests dropped %d test file(s) before classification; they carry no verdict.\n",
			response.Counts.ExcludedTests)
	}

	violationsProduction, violationsTest := splitByTestRole(response.Invalidated)
	followUp, fallout := splitByTestRole(response.AtRisk)
	violationPackages := collapsePivotPackages(violationsTest)
	falloutPackages := collapsePivotPackages(fallout)

	fmt.Fprintf(out, "\n%-22s %d direct violation(s)", "Required remediation:", len(response.Invalidated))
	if len(violationsTest) > 0 {
		fmt.Fprintf(out, " (%d production, %d test across %d package(s))",
			len(violationsProduction), len(violationsTest), len(violationPackages))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-22s %d at-risk production file(s)\n", "Production follow-up:", len(followUp))
	fmt.Fprintf(out, "%-22s %d file(s) across %d package(s)\n", "Test fallout:", len(fallout), len(falloutPackages))
	fmt.Fprintf(out, "%-22s %d unreached\n", "Coverage gaps:", len(response.Unreached))

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
		fmt.Fprintf(out, "\nREQUIRED REMEDIATION (%d) — breaks the constraint directly, must be replaced or removed:\n",
			len(response.Invalidated))
		for _, file := range violationsProduction {
			writePivotFileLine(out, file)
		}
		if len(violationPackages) > 0 {
			fmt.Fprintf(out, "  test files carrying the same violation (%d across %d package(s)):\n",
				len(violationsTest), len(violationPackages))
			writePivotPackageLines(out, "  ", violationPackages)
		}
	}

	if len(followUp) > 0 {
		fmt.Fprintf(out, "\nPRODUCTION FOLLOW-UP (%d) — no violation of its own, but built on invalidated code.\n", len(followUp))
		fmt.Fprintf(out, "Re-verify each after the required remediation lands, nearest first:\n")
		for _, file := range followUp {
			writePivotFileLine(out, file)
		}
	}

	if len(fallout) > 0 {
		fmt.Fprintf(out, "\nTEST FALLOUT (%d file(s) across %d package(s)) — collapsed on purpose.\n",
			len(fallout), len(falloutPackages))
		fmt.Fprintf(out, "These are covered by running the package, not by reading the list:\n")
		writePivotPackageLines(out, "", falloutPackages)
	}

	if len(response.Unreached) > 0 {
		gaps := collapsePivotPaths(response.Unreached)
		fmt.Fprintf(out, "\nCOVERAGE GAPS (%d) — %s\n", len(response.Unreached), response.UnreachedReason)
		for _, group := range gaps {
			fmt.Fprintf(out, "- %-40s %4d file(s)\n", group.Package, group.Files)
		}
	}

	if len(response.Unverified) > 0 {
		fmt.Fprintf(out, "\nUNVERIFIED (%d) — %s\n", len(response.Unverified), response.UnverifiedReason)
		for _, file := range response.Unverified {
			fmt.Fprintf(out, "- %-50s [%s] parser reported %s\n", file.Path, file.Role, file.AnalysisNote)
		}
	}

	if len(response.Safe) > 0 {
		fmt.Fprintf(out, "\nSAFE (%d) — no dependency path to invalidated code, and the file parsed cleanly.\n", len(response.Safe))
	}

	// Static analysis reads source without running it, so a call made through an
	// interface or reflection leaves no edge to follow. Saying so is not a
	// disclaimer: a reader who trusts a Safe verdict absolutely will eventually
	// ship a break this tool could never have seen.
	fmt.Fprintf(out, "\nVerify before acting: Safe means no path was found, not that none exists.\n")
	fmt.Fprintf(out, "Calls through interfaces, reflection, or generated code leave no edge to follow.\n")
}

// pivotResolutionWord keeps the graph's own vocabulary readable when it is
// absent. An edge with no resolution recorded is not "exact"; it is unstated,
// and printing an empty string invites the reader to fill the gap themselves.
func pivotResolutionWord(resolution string) string {
	if resolution == "" {
		return "unstated"
	}
	return resolution
}

// splitByTestRole divides a section into the work a person opens files to do and
// the work a test command covers, each already ranked for reading order.
func splitByTestRole(files []pivotFile) (production, tests []pivotFile) {
	for _, file := range files {
		if isTestRole(file.Role) {
			tests = append(tests, file)
			continue
		}
		production = append(production, file)
	}
	sort.SliceStable(production, func(a, b int) bool {
		if production[a].Distance != production[b].Distance {
			return production[a].Distance < production[b].Distance
		}
		if roleRank(production[a].Role) != roleRank(production[b].Role) {
			return roleRank(production[a].Role) < roleRank(production[b].Role)
		}
		return production[a].Path < production[b].Path
	})
	return production, tests
}

func writePivotPackageLines(out io.Writer, indent string, groups []pivotPackageGroup) {
	for _, group := range groups {
		fmt.Fprintf(out, "%s- %-34s %4d file(s)   %s\n", indent, group.Package, group.Files, group.Command)
	}
}

// pivotRepoName names the repository even when it was addressed relatively, so a
// run made with "--repo ." does not report itself against a bare dot.
func pivotRepoName(repo string) string {
	name := path.Base(repo)
	if name != "." && name != "/" && name != "" {
		return name
	}
	if absolute, err := filepath.Abs(repo); err == nil {
		return filepath.Base(absolute)
	}
	return repo
}

// collapsePivotPaths groups bare paths by directory, for sections that carry no
// verdict detail worth printing per file.
func collapsePivotPaths(paths []string) []pivotPackageGroup {
	files := make([]pivotFile, 0, len(paths))
	for _, filePath := range paths {
		files = append(files, pivotFile{Path: filePath, Distance: -1})
	}
	return collapsePivotPackages(files)
}

// writePivotFileLine prints one file the way it has to be acted on: the path, the
// role that says what kind of change it is, the plain-language reading of the
// evidence, and then the evidence itself so the claim can be checked rather than
// trusted.
func writePivotFileLine(out io.Writer, file pivotFile) {
	marker := ""
	if file.InCheckpoint {
		marker = " *"
	}
	distance := ""
	if file.Distance > 0 {
		distance = fmt.Sprintf(" (distance %d)", file.Distance)
	}
	fmt.Fprintf(out, "- %s [%s]%s%s\n", file.Path, file.Role, distance, marker)
	fmt.Fprintf(out, "    %s\n", file.Why)
	if file.VerificationRequired {
		// The fallback the card asks for, stated against the file it applies to
		// rather than once in a footer the reader has to remember.
		fmt.Fprintf(out, "    VERIFY: %s evidence — confirm in source or by test before acting on this verdict.\n",
			file.EvidenceQuality)
	}
	for _, edge := range file.Evidence {
		// The line belongs to the file being reported, not to the target it reaches:
		// it is where THIS file makes the import or the call. Printing it against the
		// target produced locations that do not exist -- a CALLS edge into a 413-line
		// regexp.go was cited as "regexp.go:676", when 676 was the call site in its
		// 828-line caller. A reader who opens the cited location and finds nothing
		// there stops believing the rest of the evidence, which is the whole point of
		// printing it.
		origin := file.Path
		if edge.Line > 0 {
			origin = fmt.Sprintf("%s:%d", file.Path, edge.Line)
		}
		// The tier leads the line because it is what decides whether the rest of
		// it can be acted on. Confidence stays, but it is no longer the only
		// thing distinguishing an import the parser read from a call it matched
		// by name -- those were previously rendered identically.
		fmt.Fprintf(out, "    [%s] evidence: %s %s -> %s (confidence %.2f, resolution %s) %s\n",
			edge.Tier, edge.Relation, origin, edge.Target, edge.Confidence,
			pivotResolutionWord(edge.Resolution), edge.Reason)
	}
}
