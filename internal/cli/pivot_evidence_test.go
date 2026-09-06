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
