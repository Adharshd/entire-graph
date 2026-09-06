package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// pivotPartialAnalysisSnapshot is the fixture for incomplete analysis: a repository
// shaped like the ones static analysis cannot fully resolve, with one file per
// pattern the parser loses.
//
//	direct.go        imports the prohibited package outright     -> CONFIRMED
//	dispatch.go      calls it through an interface, so the edge
//	                 resolved by name only                       -> HEURISTIC
//	zz_generated.go  machine-written, parsed with a warning      -> UNVERIFIED
//	broken.go        the parser failed on it, and it reaches
//	                 nothing invalidated                         -> UNVERIFIED, not SAFE
//	clean.go         parsed fine, reaches nothing                -> SAFE
//
// broken.go is the load-bearing case. Every other file here is about labelling
// evidence that exists; broken.go is about a verdict drawn from evidence that
// could not be collected. Before evidence tiers it was reported SAFE, which reads
// as "checked and clear" when the truth was "never successfully read".
func pivotPartialAnalysisSnapshot() sem.ProviderSnapshot {
	file := func(path string) sem.FileRecord {
		return sem.FileRecord{ID: "repo:file:" + path, Path: path, Language: "go"}
	}
	symbol := func(name, path string) sem.SymbolRecord {
		return sem.SymbolRecord{ID: "repo:sym:" + name, Name: name, FilePath: path}
	}
	return sem.ProviderSnapshot{
		Header: sem.SnapshotHeader{
			SchemaVersion: "1.0",
			Provider:      "test",
			LanguageTiers: map[string]string{"go": "semantic"},
			// The two diagnostic channels pivot used to copy into its response
			// and never read. They are the whole reason this fixture exists.
			PartialFailures: []sem.PartialFailure{{
				Code:                 "E_PARSE_ERROR",
				Severity:             "error",
				FilePath:             "broken.go",
				EffectOnCompleteness: "file record emitted but symbol parsing skipped",
			}},
			Warnings: []sem.ProviderWarning{{
				Code:                 "W_GENERATED_SOURCE",
				Severity:             "warning",
				FilePath:             "zz_generated.go",
				EffectOnCompleteness: "generated source parsed without its generator's inputs",
			}},
		},
		Files: []sem.FileRecord{
			file("direct.go"), file("dispatch.go"), file("zz_generated.go"),
			file("broken.go"), file("clean.go"),
		},
		Symbols: []sem.SymbolRecord{
			symbol("Direct", "direct.go"), symbol("Dispatch", "dispatch.go"),
			symbol("Generated", "zz_generated.go"), symbol("Broken", "broken.go"),
			symbol("Clean", "clean.go"),
		},
		Relations: []sem.RelationRecord{
			// An import declaration read straight out of the source. The target
			// resolved by name only, which describes the far end, not the
			// statement -- this is still the strongest evidence pivot can have.
			{FromID: "repo:file:direct.go", ToID: "external:import:os/exec", Type: "IMPORTS",
				Confidence: 0.80, Resolution: "name_only", Reason: "import declaration matched by language-specific scanner"},
			// A call the parser could only match by name. This is the edge the
			// card is about: often right, never proof, and previously rendered
			// exactly like the import above.
			{FromID: "repo:sym:Dispatch", ToID: "repo:sym:Direct", Type: "CALLS",
				Confidence: 0.68, Resolution: "name_only", Reason: "method call matched globally unique method name"},
			// Generated code reaching invalidated code. The edge resolved
			// cleanly; the file it lives in did not.
			{FromID: "repo:sym:Generated", ToID: "repo:sym:Direct", Type: "CALLS",
				Confidence: 0.92, Resolution: "exact", Reason: "direct call expression resolved to same-file symbol"},
		},
	}
}

func pivotFileNamed(response pivotResponse, path string) (pivotFile, bool) {
	groups := [][]pivotFile{response.Invalidated, response.AtRisk, response.Safe, response.Unverified}
	for _, group := range groups {
		for _, file := range group {
			if file.Path == path {
				return file, true
			}
		}
	}
	return pivotFile{}, false
}

// TestPivotDoesNotReportUnparsedFileAsSafe is the curveball in one assertion. A
// file the parser failed on reaches nothing invalidated -- and could not have,
// since it was never read. Calling that SAFE presents a gap in the analysis as a
// finding about the code.
func TestPivotDoesNotReportUnparsedFileAsSafe(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})

	broken, found := pivotFileNamed(response, "broken.go")
	if !found {
		t.Fatal("broken.go carries no verdict at all; it should be reported, not dropped")
	}
	if broken.Verdict != verdictUnverified {
		t.Errorf("broken.go verdict = %q, want %q: the parser failed on it, so no path found is not the same as no path",
			broken.Verdict, verdictUnverified)
	}
	if !broken.VerificationRequired {
		t.Error("broken.go verification_required = false, want true: an unparsed file must not be actioned from the graph alone")
	}
	if broken.AnalysisNote != "E_PARSE_ERROR" {
		t.Errorf("broken.go analysis_note = %q, want the parser's own code E_PARSE_ERROR", broken.AnalysisNote)
	}
	// Only a file that would otherwise have been SAFE moves into the UNVERIFIED
	// bucket. zz_generated.go has a real path to invalidated code, so it stays
	// AT-RISK and carries its uncertainty as evidence quality instead -- the
	// verdict still says there is a path, because there is one.
	if response.Counts.Unverified != 1 {
		t.Errorf("counts.unverified = %d, want 1 (broken.go only)", response.Counts.Unverified)
	}
	generated, found := pivotFileNamed(response, "zz_generated.go")
	if !found || generated.Verdict != verdictAtRisk {
		t.Errorf("zz_generated.go verdict = %q, want %q: an unreadable file with a real path is still at risk",
			generated.Verdict, verdictAtRisk)
	}
	if generated.EvidenceQuality != evidenceUnverified {
		t.Errorf("zz_generated.go evidence_quality = %q, want %q", generated.EvidenceQuality, evidenceUnverified)
	}

	// The distinction only means something if the clean file keeps the clean
	// verdict. Otherwise this is a tool that calls everything uncertain.
	clean, found := pivotFileNamed(response, "clean.go")
	if !found || clean.Verdict != verdictSafe {
		t.Errorf("clean.go verdict = %q, want %q: it parsed cleanly and reaches nothing", clean.Verdict, verdictSafe)
	}
	if clean.VerificationRequired {
		t.Error("clean.go verification_required = true, want false: nothing about it is in doubt")
	}
}

// TestPivotTiersEvidenceByResolution pins the three-way split the card asks users
// and agents to be able to make.
func TestPivotTiersEvidenceByResolution(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})

	for path, want := range map[string]string{
		// An import statement the scanner read out of the file.
		"direct.go": evidenceConfirmed,
		// A call matched by name -- the graph's own words for a guess.
		"dispatch.go": evidenceHeuristic,
		// Resolved edge, unreadable file: the file decides.
		"zz_generated.go": evidenceUnverified,
	} {
		file, found := pivotFileNamed(response, path)
		if !found {
			t.Errorf("%s: not present in the response", path)
			continue
		}
		if file.EvidenceQuality != want {
			t.Errorf("%s: evidence_quality = %q, want %q", path, file.EvidenceQuality, want)
		}
		if file.EvidenceQuality == evidenceConfirmed && file.VerificationRequired {
			t.Errorf("%s: confirmed evidence must not demand verification", path)
		}
		if file.EvidenceQuality != evidenceConfirmed && !file.VerificationRequired {
			t.Errorf("%s: %s evidence must demand verification", path, file.EvidenceQuality)
		}
	}
}

// TestPivotFullyResolvedBehaviourUnchanged is the regression lock the card asks
// for in as many words: "existing behaviour for fully resolved code must continue
// to work". The original fixture has no warnings and no partial failures, so
// nothing about its verdicts may move, and nothing in it may ask to be verified.
func TestPivotFullyResolvedBehaviourUnchanged(t *testing.T) {
	response := buildPivotResponse(pivotTestSnapshot(), pivotFlags{Dependency: []string{"net/http"}, Depth: pivotMaxDepth})

	for path, want := range map[string]string{
		"handler.go":      verdictInvalidated,
		"handler_test.go": verdictInvalidated,
		"router.go":       verdictAtRisk,
		"helper_test.go":  verdictAtRisk,
		"fixtures.go":     verdictAtRisk,
		"unrelated.go":    verdictSafe,
	} {
		file, found := pivotFileNamed(response, path)
		if !found {
			t.Errorf("%s: missing from the response", path)
			continue
		}
		if file.Verdict != want {
			t.Errorf("%s: verdict = %q, want %q -- evidence tiers must not move a verdict", path, file.Verdict, want)
		}
	}
	if response.Counts.Unverified != 0 {
		t.Errorf("counts.unverified = %d on a clean snapshot, want 0", response.Counts.Unverified)
	}
	if len(response.Unverified) != 0 {
		t.Errorf("unverified bucket has %d file(s) on a clean snapshot, want none", len(response.Unverified))
	}
	// Safe files carry no evidence, so they carry no tier: the verdict is the
	// absence of a path, not weak evidence of one.
	unrelated, _ := pivotFileNamed(response, "unrelated.go")
	if unrelated.EvidenceQuality != "" {
		t.Errorf("unrelated.go evidence_quality = %q, want empty", unrelated.EvidenceQuality)
	}
	if unrelated.VerificationRequired {
		t.Error("unrelated.go verification_required = true, want false")
	}
}

// TestPivotTextTagsEvidenceForUsers covers the first of the card's two audiences.
func TestPivotTextTagsEvidenceForUsers(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	var out bytes.Buffer
	writePivotText(&out, response)
	text := out.String()

	for _, want := range []string{
		"[" + evidenceConfirmed + "] evidence:",
		"[" + evidenceHeuristic + "] evidence:",
		"resolution name_only",
		"VERIFY:",
		"UNVERIFIED (1)",
		"E_PARSE_ERROR",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report is missing %q\n---\n%s", want, text)
		}
	}
	// The old wording promised more than the verdict could deliver.
	if !strings.Contains(text, "and the file parsed cleanly") {
		t.Error("SAFE heading should say the file parsed cleanly, which is now the thing that separates it from UNVERIFIED")
	}
}

// TestPivotPlanCarriesEvidenceForAgents covers the second audience. An agent acts
// on fields, not on prose, so the distinction has to survive into the work order.
func TestPivotPlanCarriesEvidenceForAgents(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	plan := buildPivotPlan(response, pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})

	byPath := map[string]pivotWorkItem{}
	for _, item := range plan.WorkItems {
		byPath[item.Path] = item
	}
	for path, want := range map[string]string{
		"direct.go":       evidenceConfirmed,
		"dispatch.go":     evidenceHeuristic,
		"zz_generated.go": evidenceUnverified,
	} {
		item, found := byPath[path]
		if !found {
			t.Errorf("%s: no work item", path)
			continue
		}
		if item.EvidenceQuality != want {
			t.Errorf("%s: work item evidence_quality = %q, want %q", path, item.EvidenceQuality, want)
		}
		if want != evidenceConfirmed && !item.VerificationRequired {
			t.Errorf("%s: work item must set verification_required for %s evidence", path, want)
		}
		for _, edge := range item.Evidence {
			if edge.Tier == "" {
				t.Errorf("%s: an evidence edge reached the plan with no tier", path)
			}
		}
	}

	// A blind spot an agent never reads is not a blind spot. Both new ones are
	// stated before the work items, where the decision to trust is made.
	spots := map[string]bool{}
	for _, spot := range plan.KnownBlindSpots {
		spots[spot.ID] = true
	}
	for _, want := range []string{"heuristic-edges", "unverified-parse"} {
		if !spots[want] {
			t.Errorf("known_blind_spots is missing %q", want)
		}
	}
	contract := strings.Join(plan.ExecutionContract.MustNotClaim, "\n")
	if !strings.Contains(contract, "HEURISTIC edge") {
		t.Error("execution contract must forbid treating a heuristic edge as an established fact")
	}
}

// TestClassifyEvidenceTier states the rules directly, so a change to the tier
// boundaries has to be made on purpose rather than fall out of a refactor.
func TestClassifyEvidenceTier(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		relation     string
		resolution   string
		confidence   float64
		warningCodes []string
		filePath     string
		want         string
	}{
		{"import read from source", "IMPORTS", "name_only", 0.80, nil, "a.go", evidenceConfirmed},
		{"call followed to a definition", "CALLS", "exact", 0.92, nil, "a.go", evidenceConfirmed},
		{"call resolved through an import path", "CALLS", "import_resolved", 0.84, nil, "a.go", evidenceConfirmed},
		{"call matched by name", "CALLS", "name_only", 0.68, nil, "a.go", evidenceHeuristic},
		{"call attributed to a package", "CALLS", "package", 0.80, nil, "a.go", evidenceHeuristic},
		{"receiver type inferred", "CALLS", "type_inferred", 0.90, nil, "a.go", evidenceHeuristic},
		{"framework pattern", "CALLS", "pattern", 0.70, nil, "a.go", evidenceHeuristic},
		{"resolution unstated", "CALLS", "", 0.99, nil, "a.go", evidenceHeuristic},
		{"resolved but flagged weak", "CALLS", "exact", 0.40, nil, "a.go", evidenceHeuristic},
		{"edge carries a warning", "CALLS", "exact", 0.92, []string{"W_TRUNCATED"}, "a.go", evidenceUnverified},
		{"file did not parse", "CALLS", "exact", 0.92, nil, "broken.go", evidenceUnverified},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			incomplete := map[string]string{"broken.go": "E_PARSE_ERROR"}
			got := classifyEvidenceTier(testCase.relation, testCase.resolution, testCase.confidence,
				testCase.warningCodes, testCase.filePath, incomplete)
			if got != testCase.want {
				t.Errorf("tier = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestWeakestEvidenceTierGovernsTheFile pins the summarising rule: a verdict is
// only as trustworthy as the shakiest edge holding it up, so one heuristic edge
// among confirmed ones decides the file.
func TestWeakestEvidenceTierGovernsTheFile(t *testing.T) {
	if got := weakestEvidenceTier(nil); got != "" {
		t.Errorf("no evidence should yield no tier, got %q", got)
	}
	mixed := []pivotEdge{{Tier: evidenceConfirmed}, {Tier: evidenceHeuristic}, {Tier: evidenceConfirmed}}
	if got := weakestEvidenceTier(mixed); got != evidenceHeuristic {
		t.Errorf("mixed evidence = %q, want %q", got, evidenceHeuristic)
	}
	worst := []pivotEdge{{Tier: evidenceHeuristic}, {Tier: evidenceUnverified}}
	if got := weakestEvidenceTier(worst); got != evidenceUnverified {
		t.Errorf("with an unverified edge = %q, want %q", got, evidenceUnverified)
	}
}

// TestProhibitedMatchResolvesInternalPackagePaths covers the constraint shape the
// matcher used to answer with silence: an architecture rule naming an internal
// package rather than a third-party import.
//
// The two id shapes are the ones the graph actually emits, taken from
// `edges --relation IMPORTS,CALLS` against this repository, not invented:
//
//	local/entire-graph:file:internal/sem/analyze.go
//	local/entire-graph:Go:internal/sem/provider.go:function:StreamSnapshot
//
// Before this, --dependency internal/sem returned 0 invalidated files on a
// repository where 139 files depend on it. Zero reads as "you comply"; it meant
// "I did not understand the question", which is the failure mode this whole
// change is about.
func TestProhibitedMatchResolvesInternalPackagePaths(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		toID       string
		prohibited string
		want       string
	}{
		{"file record in an internal package", "local/entire-graph:file:internal/sem/analyze.go", "internal/sem", "internal/sem"},
		{"symbol in an internal package", "local/entire-graph:Go:internal/sem/provider.go:function:StreamSnapshot", "internal/sem", "internal/sem"},
		{"method in an internal package", "local/entire-graph:Go:internal/sem/records_cache.go:method:Cache.Load", "internal/sem", "internal/sem"},
		{"a sibling package must not match", "local/entire-graph:file:internal/cli/pivot.go", "internal/sem", ""},
		{"a prefix that is not a path segment must not match", "local/entire-graph:file:internal/semantics/x.go", "internal/sem", ""},

		// The external cases the matcher already handled. They are here because
		// the new rule must not disturb them: os/exec never appears as a path
		// segment inside an internal id, so nothing about these moves.
		{"bare external import", "external:import:os/exec", "os/exec", "os/exec"},
		{"external import below the package", "external:import:os/exec/internal/x", "os/exec", "os/exec"},
		{"unrelated external import", "external:import:net/http", "os/exec", ""},
		{"internal file against an external constraint", "local/entire-graph:file:internal/sem/analyze.go", "os/exec", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := prohibitedMatch(testCase.toID, []string{testCase.prohibited}); got != testCase.want {
				t.Errorf("prohibitedMatch(%q, %q) = %q, want %q", testCase.toID, testCase.prohibited, got, testCase.want)
			}
		})
	}
}

// TestPlanForbidsTrivialSatisfaction covers the failure mode the verification loop
// hit before the curveball: `invalidated == 0` is satisfied by deleting the code
// as well as by fixing it. The loop caught it by hand on gorilla/mux -- one file,
// +7/-8, no test touched -- and nothing in the plan said it had to.
//
// must_not_claim binds the report. These bind the work, which is a different
// thing: deleting a file is not a false claim, it is a false fix.
func TestPlanForbidsTrivialSatisfaction(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	plan := buildPivotPlan(response, pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})

	rules := strings.Join(plan.ExecutionContract.MustNotDo, "\n")
	for _, want := range []struct{ name, fragment string }{
		{"deletion", "Must not delete production code"},
		{"test removal", "delete, skip, or weaken a test"},
		{"moving the goalposts", "Must not change the constraint to fit the result"},
		{"narrowing the analysed set", "Must not narrow the analysed set"},
		{"unverified suppression", "suppressing the parser diagnostic"},
		{"diff is the other half of the evidence", "git diff --stat"},
	} {
		if !strings.Contains(rules, want.fragment) {
			t.Errorf("must_not_do is missing the %s invariant (%q)\n---\n%s", want.name, want.fragment, rules)
		}
	}

	// A clean snapshot has no unverified files, so that clause must not appear --
	// a contract padded with clauses that do not apply teaches an agent to skim it.
	clean := buildPivotResponse(pivotTestSnapshot(), pivotFlags{Dependency: []string{"net/http"}, Depth: pivotMaxDepth})
	cleanPlan := buildPivotPlan(clean, pivotFlags{Dependency: []string{"net/http"}, Depth: pivotMaxDepth})
	if strings.Contains(strings.Join(cleanPlan.ExecutionContract.MustNotDo, "\n"), "suppressing the parser diagnostic") {
		t.Error("the unverified clause appears on a snapshot with no unverified files")
	}

	notes := strings.Join(cleanPlan.Validation.Notes, "\n")
	for _, want := range []string{"clean checkout", "CANNOT_VERIFY", "satisfied by deleting the code"} {
		if !strings.Contains(notes, want) {
			t.Errorf("validation notes are missing %q\n---\n%s", want, notes)
		}
	}
}

// TestPlanEmitsCheckableInvariants covers the half of the contract a verifier can
// evaluate. must_not_do binds an agent that reads it; these bind one that does
// not, which is the case the verification loop actually hit -- `invalidated == 0`
// is satisfied by deleting the code, and prose does not stop that.
func TestPlanEmitsCheckableInvariants(t *testing.T) {
	response := buildPivotResponse(pivotPartialAnalysisSnapshot(), pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	plan := buildPivotPlan(response, pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	invariants := plan.Validation.Invariants

	// direct.go is the invalidated production file. Deleting it removes the
	// violation along with the code, which is the attack this pins.
	if len(invariants.FilesMustStillExist) != 1 || invariants.FilesMustStillExist[0] != "direct.go" {
		t.Errorf("files_must_still_exist = %v, want [direct.go]", invariants.FilesMustStillExist)
	}
	if invariants.MaxNetLinesDeleted <= 0 {
		t.Errorf("max_net_lines_deleted = %d, want a positive budget", invariants.MaxNetLinesDeleted)
	}
	// A bare number invites a reader to argue with it. The derivation travels
	// with it so a consumer can see what it was calibrated on.
	if !strings.Contains(invariants.MaxNetLinesDeletedBasis, "gorilla/mux") {
		t.Errorf("max_net_lines_deleted_basis does not say where the number came from: %q", invariants.MaxNetLinesDeletedBasis)
	}
	if invariants.ExpectedAfter.Invalidated != 0 {
		t.Errorf("expected_after.invalidated = %d, want 0", invariants.ExpectedAfter.Invalidated)
	}
	// Silencing a parser diagnostic would move a file out of UNVERIFIED without
	// anyone having read it, so the ceiling is the count we started with.
	if invariants.ExpectedAfter.UnverifiedMustNotIncrease != response.Counts.Unverified {
		t.Errorf("unverified ceiling = %d, want %d", invariants.ExpectedAfter.UnverifiedMustNotIncrease, response.Counts.Unverified)
	}
	if len(invariants.FlagsMustMatch.Dependency) != 1 || invariants.FlagsMustMatch.Dependency[0] != "os/exec" {
		t.Errorf("flags_must_match.dependency = %v, want [os/exec]", invariants.FlagsMustMatch.Dependency)
	}

	// The honest half. A verifier whose uncheckable list came back empty would be
	// claiming full coverage, which no check over a diff can have -- and it is
	// what makes a clean result PARTIAL rather than PASS.
	if len(invariants.NotMechanicallyCheckable) == 0 {
		t.Fatal("not_mechanically_checkable is empty; a deterministic checker cannot decide behaviour preservation and must say so")
	}
	uncheckable := strings.Join(invariants.NotMechanicallyCheckable, "\n")
	for _, want := range []string{"preserves behaviour", "HEURISTIC evidence", "UNVERIFIED"} {
		if !strings.Contains(uncheckable, want) {
			t.Errorf("not_mechanically_checkable does not mention %q\n---\n%s", want, uncheckable)
		}
	}
}

// TestInvariantsNeverPinFilesTheAgentIsToldNotToEdit stops the contract
// contradicting itself: the plan tells an agent a GENERATED file is fixed by its
// generator and a VENDOR file by changing the dependency, so requiring either to
// survive unchanged would forbid the remedy the plan just prescribed.
func TestInvariantsNeverPinFilesTheAgentIsToldNotToEdit(t *testing.T) {
	snapshot := pivotPartialAnalysisSnapshot()
	// zz_generated.go is already in the fixture and classified GENERATED by the
	// role pass only if named conventionally; pin the behaviour directly instead.
	response := buildPivotResponse(snapshot, pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	for index := range response.Invalidated {
		response.Invalidated[index].Role = roleGenerated
	}
	plan := buildPivotPlan(response, pivotFlags{Dependency: []string{"os/exec"}, Depth: pivotMaxDepth})
	if len(plan.Validation.Invariants.FilesMustStillExist) != 0 {
		t.Errorf("files_must_still_exist = %v, want none: every violation is in a GENERATED file, which the plan says is not fixed in place",
			plan.Validation.Invariants.FilesMustStillExist)
	}
}

// TestDeletionBudgetScalesWithTheWork pins the derivation rather than the number,
// so changing the calibration is a deliberate act with a visible reason.
func TestDeletionBudgetScalesWithTheWork(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		violations int
		want       int
	}{
		// A run with no production violations still needs slack: violations
		// confined to test files can legitimately move a few lines.
		{"no production violations still gets the floor", 0, deletionBudgetHeadroom},
		{"one file, calibrated on the mux fix", 1, observedNetLinesPerReplacedFile * deletionBudgetHeadroom},
		{"budget grows with the number of files to fix", 8, 8 * observedNetLinesPerReplacedFile * deletionBudgetHeadroom},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := pivotDeletionBudget(testCase.violations); got != testCase.want {
				t.Errorf("pivotDeletionBudget(%d) = %d, want %d", testCase.violations, got, testCase.want)
			}
		})
	}
}

// TestFromScopesTheRulesSubject covers the half of a constraint the command could
// not previously express. A constraint is "X may no longer reach Y": --dependency
// is Y, and --from is X. Without it every rule was repo-wide, which is right for a
// vendor removal and wrong for an architecture boundary -- "the API layer may not
// touch storage" is a statement about the API layer, and answering it with every
// violation in the repository buries the answer among files the rule does not
// govern.
func TestFromScopesTheRulesSubject(t *testing.T) {
	// handler.go and handler_test.go both import net/http. Scoping to handler.go's
	// directory is not interesting in this fixture (everything is at the root), so
	// scope by file path, which is the same predicate.
	scoped := buildPivotResponse(pivotTestSnapshot(), pivotFlags{
		Dependency: []string{"net/http"},
		Depth:      pivotMaxDepth,
		From:       []string{"handler.go"},
	})
	if got := pivotVerdict(scoped, "handler.go"); got != verdictInvalidated {
		t.Errorf("handler.go is inside the scope: verdict = %q, want %q", got, verdictInvalidated)
	}
	// handler_test.go breaks the same rule, but the rule was not addressed to it.
	if got := pivotVerdict(scoped, "handler_test.go"); got == verdictInvalidated {
		t.Error("handler_test.go is outside --from and must not be reported as violating this rule")
	}

	// Propagation is deliberately NOT scoped. router.go stands on handler.go, which
	// has to change, and it stands there whatever directory it lives in.
	if got := pivotVerdict(scoped, "router.go"); got != verdictAtRisk {
		t.Errorf("router.go: verdict = %q, want %q -- a dependent is at risk wherever it lives", got, verdictAtRisk)
	}

	// Omitting --from must behave exactly as before.
	unscoped := buildPivotResponse(pivotTestSnapshot(), pivotFlags{
		Dependency: []string{"net/http"},
		Depth:      pivotMaxDepth,
	})
	if got := pivotVerdict(unscoped, "handler_test.go"); got != verdictInvalidated {
		t.Errorf("without --from, handler_test.go = %q, want %q", got, verdictInvalidated)
	}
	if unscoped.Counts.Invalidated <= scoped.Counts.Invalidated {
		t.Errorf("scoping must narrow the result: unscoped %d, scoped %d",
			unscoped.Counts.Invalidated, scoped.Counts.Invalidated)
	}
}

// TestWithinPivotScopeMatchesPathSegments pins the boundary rule directly. A raw
// prefix test would make `--from internal/cli` silently cover internal/climate.go,
// which is the same mistake prohibitedMatch had to avoid on the other end of the
// relation.
func TestWithinPivotScopeMatchesPathSegments(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		path  string
		scope []string
		want  bool
	}{
		{"empty scope is the whole repository", "anything/at/all.go", nil, true},
		{"file inside the scope", "internal/cli/pivot.go", []string{"internal/cli"}, true},
		{"the scope directory itself", "internal/cli", []string{"internal/cli"}, true},
		{"trailing slash is tolerated", "internal/cli/pivot.go", []string{"internal/cli/"}, true},
		{"a sibling directory does not match", "internal/sem/provider.go", []string{"internal/cli"}, false},
		{"a prefix that is not a path segment does not match", "internal/climate.go", []string{"internal/cli"}, false},
		{"any one of several scopes is enough", "internal/sem/provider.go", []string{"internal/cli", "internal/sem"}, true},
		{"outside every scope", "cmd/main.go", []string{"internal/cli", "internal/sem"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := withinPivotScope(testCase.path, testCase.scope); got != testCase.want {
				t.Errorf("withinPivotScope(%q, %v) = %v, want %v", testCase.path, testCase.scope, got, testCase.want)
			}
		})
	}
}
