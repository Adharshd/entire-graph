package cli

import (
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// pivotTestSnapshot is the smallest graph that exercises both things
// --exclude-tests has to get right:
//
//	handler.go        imports the prohibited package  -> INVALIDATED
//	handler_test.go   imports it too                  -> INVALIDATED, but test-only
//	router.go         calls handler.go                -> AT-RISK
//	helper_test.go    calls handler_test.go           -> AT-RISK, but test-only
//	fixtures.go       calls helper_test.go            -> AT-RISK only through a test
//	unrelated.go      touches none of it              -> SAFE
//
// fixtures.go is the case a filter applied to the finished report would get
// wrong: it is production code, but its only route to anything invalidated runs
// through a test file, so once tests are excluded nothing invalidated reaches it.
func pivotTestSnapshot() sem.ProviderSnapshot {
	file := func(path string) sem.FileRecord {
		return sem.FileRecord{ID: "repo:file:" + path, Path: path, Language: "go"}
	}
	symbol := func(name, path string) sem.SymbolRecord {
		return sem.SymbolRecord{ID: "repo:sym:" + name, Name: name, FilePath: path}
	}
	relation := func(from, to, kind string) sem.RelationRecord {
		return sem.RelationRecord{FromID: from, ToID: to, Type: kind, Confidence: 0.8, Reason: "test fixture"}
	}
	return sem.ProviderSnapshot{
		Header: sem.SnapshotHeader{
			SchemaVersion: "1.0",
			Provider:      "test",
			LanguageTiers: map[string]string{"go": "semantic"},
		},
		Files: []sem.FileRecord{
			file("handler.go"), file("handler_test.go"), file("router.go"),
			file("helper_test.go"), file("fixtures.go"), file("unrelated.go"),
		},
		Symbols: []sem.SymbolRecord{
			symbol("Handle", "handler.go"), symbol("HandleTest", "handler_test.go"),
			symbol("Route", "router.go"), symbol("Helper", "helper_test.go"),
			symbol("Fixture", "fixtures.go"), symbol("Unrelated", "unrelated.go"),
		},
		Relations: []sem.RelationRecord{
			relation("repo:file:handler.go", "external:import:net/http", "IMPORTS"),
			relation("repo:file:handler_test.go", "external:import:net/http", "IMPORTS"),
			relation("repo:sym:Route", "repo:sym:Handle", "CALLS"),
			relation("repo:sym:Helper", "repo:sym:HandleTest", "CALLS"),
			relation("repo:sym:Fixture", "repo:sym:Helper", "CALLS"),
		},
	}
}

func pivotVerdict(response pivotResponse, path string) string {
	// response.Unverified is in this list because leaving it out would make an
	// unparsed file look like one that was never classified at all, which is the
	// exact confusion evidence tiers exist to remove.
	for _, group := range [][]pivotFile{response.Invalidated, response.AtRisk, response.Safe, response.Unverified} {
		for _, file := range group {
			if file.Path == path {
				return file.Verdict
			}
		}
	}
	return ""
}

// TestPivotClassifiesTestFilesWithoutExclusion is the baseline the flag is meant
// to trim: every test file is classified, and production code reachable only
// through a test file is dragged along with it.
func TestPivotClassifiesTestFilesWithoutExclusion(t *testing.T) {
	response := buildPivotResponse(pivotTestSnapshot(), pivotFlags{Dependency: []string{"net/http"}, Depth: pivotMaxDepth})

	for path, want := range map[string]string{
		"handler.go":      verdictInvalidated,
		"handler_test.go": verdictInvalidated,
		"router.go":       verdictAtRisk,
		"helper_test.go":  verdictAtRisk,
		"fixtures.go":     verdictAtRisk,
		"unrelated.go":    verdictSafe,
	} {
		if got := pivotVerdict(response, path); got != want {
			t.Errorf("%s: verdict = %q, want %q", path, got, want)
		}
	}
	if response.Counts.ExcludedTests != 0 {
		t.Errorf("excluded_tests = %d without the flag, want 0", response.Counts.ExcludedTests)
	}
}

// TestPivotExcludeTestsDropsTestsAndTheirPropagation pins the part that a filter
// over the finished report would not give you: excluded test files carry no
// verdict AND stop conducting At-Risk to the production files behind them.
func TestPivotExcludeTestsDropsTestsAndTheirPropagation(t *testing.T) {
	response := buildPivotResponse(pivotTestSnapshot(), pivotFlags{Dependency: []string{"net/http"}, Depth: pivotMaxDepth, ExcludeTests: true})

	for path, want := range map[string]string{
		"handler.go":   verdictInvalidated,
		"router.go":    verdictAtRisk,
		"fixtures.go":  verdictSafe,
		"unrelated.go": verdictSafe,
	} {
		if got := pivotVerdict(response, path); got != want {
			t.Errorf("%s: verdict = %q, want %q", path, got, want)
		}
	}
	for _, path := range []string{"handler_test.go", "helper_test.go"} {
		if got := pivotVerdict(response, path); got != "" {
			t.Errorf("%s: classified as %q, want no verdict at all", path, got)
		}
	}

	if response.Counts.ExcludedTests != 2 {
		t.Errorf("excluded_tests = %d, want 2", response.Counts.ExcludedTests)
	}
	// The excluded files are accounted for rather than silently vanishing: the
	// classified total plus the exclusions is still every file in the snapshot.
	if total := response.Counts.Total + response.Counts.ExcludedTests; total != 6 {
		t.Errorf("classified %d + excluded %d = %d, want 6 (every file in the snapshot)",
			response.Counts.Total, response.Counts.ExcludedTests, total)
	}
	if !response.ExcludeTests {
		t.Error("response does not record that --exclude-tests was in effect")
	}
}

// TestParsePivotFlagsExcludeTests pins the flag itself, which is off by default.
func TestParsePivotFlagsExcludeTests(t *testing.T) {
	flags, err := parsePivotFlags([]string{"--dependency", "net/http"})
	if err != nil {
		t.Fatalf("parsePivotFlags: %v", err)
	}
	if flags.ExcludeTests {
		t.Error("ExcludeTests defaults to true, want false")
	}

	flags, err = parsePivotFlags([]string{"--dependency", "net/http", "--exclude-tests"})
	if err != nil {
		t.Fatalf("parsePivotFlags: %v", err)
	}
	if !flags.ExcludeTests {
		t.Error("--exclude-tests did not set ExcludeTests")
	}
}
