package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/entire-graph/internal/sem"
)

// Evidence tiers separate what the graph proved from what it guessed.
//
// Before this existed, pivot carried each edge's confidence into the report and
// then never consulted it: the number was printed by writePivotFileLine and read
// by nothing. Two edges that mean very different things were rendered and treated
// identically, and a P0 work item could rest entirely on the weaker one with
// nothing saying so.
//
// The tiers are not a new opinion about the code. They are the graph's own
// resolution vocabulary, grouped by whether the relation was resolved to a
// declaration or inferred from a name.
const (
	// evidenceConfirmed is a relation the parser resolved structurally: an import
	// declaration it read, or a call it followed to a definition.
	evidenceConfirmed = "CONFIRMED"
	// evidenceHeuristic is a relation inferred rather than resolved -- matched by
	// name, by package, or by an inferred receiver type. Often right, never proof.
	evidenceHeuristic = "HEURISTIC"
	// evidenceUnverified is a relation whose file the parser could not fully read.
	// The edge may be right; the absence of other edges in that file is unknown,
	// which is the part that makes a Safe verdict unsafe.
	evidenceUnverified = "UNVERIFIED"
)

// confirmedResolutions are the graph's words for "I followed this to a
// declaration" as opposed to "I matched a name".
//
// Measured on this repository (57,791 relations), the resolution vocabulary and
// the confidence attached to each is:
//
//	CALLS      import_external  0.78   8250   inferred: call attributed via an import
//	CALLS      exact            0.92   7248   resolved: followed to a definition
//	CALLS      package          0.80   5548   inferred: package-level attribution
//	IMPORTS    name_only        0.80   2170   an import declaration read from source
//	CALLS      name_only        0.68    761   inferred: globally unique method name
//	CALLS      import_resolved  0.84    254   resolved through an import path
//	CALLS      type_inferred    0.90    208   inferred from a receiver type
//	IMPORTS    import_resolved  0.93    146   resolved: import declaration + target
//	CALLS      pattern          0.70    123   inferred from a framework pattern
//
// Two things fall out of that table, and both shaped these rules.
//
// First, confidence alone cannot carry the distinction. 0.80 covers both
// `IMPORTS name_only` -- an import statement physically present in the source --
// and `CALLS package`, which is a guess about which package a call landed in.
// A threshold over confidence would put a fact and an inference in the same tier.
// Resolution is the axis that separates them, so the tiers key on resolution and
// use confidence only as a floor.
//
// Second, no relation in this repository carries confidence 1.00. The highest is
// 0.95. Any design that waited for certainty would tier everything as uncertain.
var confirmedResolutions = map[string]bool{
	"exact":           true,
	"import_resolved": true,
	"full":            true,
	"resolved":        true,
	"signature":       true,
}

// confirmedByRelation are relation types that are read directly out of the source
// text rather than inferred, whatever the parser managed to resolve the target to.
//
// IMPORTS is here because of the 2,170 `IMPORTS name_only` edges above. The
// `name_only` describes the target: the parser did not bind the imported package
// to a symbol it had parsed. It does not describe the import statement, which was
// read from the file. For a prohibited-dependency question that statement is the
// strongest evidence there is -- it is the line a reviewer would grep for -- so
// grading it HEURISTIC because the far end went unresolved would understate it.
var confirmedByRelation = map[string]bool{
	"IMPORTS": true,
}

// confirmedConfidenceFloor is a floor, not a threshold. Resolution decides the
// tier; this only stops an edge the parser resolved but then flagged as weak from
// being presented as confirmed. Set below the 0.84 of `CALLS import_resolved` so
// it does not silently demote a whole resolution class.
const confirmedConfidenceFloor = 0.80

// classifyEvidenceTier grades one edge. incompleteFiles are the files the parser
// reported a warning or partial failure against; an edge attached to one of those
// is UNVERIFIED regardless of how well it resolved, because the problem is not
// that edge but the edges that may be missing beside it.
func classifyEvidenceTier(relationType, resolution string, confidence float64, warningCodes []string, filePath string, incompleteFiles map[string]string) string {
	if len(warningCodes) > 0 {
		return evidenceUnverified
	}
	if _, incomplete := incompleteFiles[filePath]; incomplete {
		return evidenceUnverified
	}
	if confirmedByRelation[relationType] {
		return evidenceConfirmed
	}
	if confirmedResolutions[resolution] && confidence >= confirmedConfidenceFloor {
		return evidenceConfirmed
	}
	return evidenceHeuristic
}

// tierRank orders the tiers weakest-last so a file can be summarised by its worst
// piece of evidence. A verdict is only as good as the shakiest edge holding it up.
func tierRank(tier string) int {
	switch tier {
	case evidenceConfirmed:
		return 0
	case evidenceHeuristic:
		return 1
	case evidenceUnverified:
		return 2
	default:
		return 1
	}
}

// weakestEvidenceTier summarises a file's evidence by its weakest edge. A file
// held up by one confirmed import and one name-matched call is reported at the
// name-matched end, because that is the claim a reader has to check.
//
// A file with no evidence at all -- Safe, or Unreached -- returns "", and the
// caller decides what that means. Safe with no evidence is not weak evidence; it
// is the absence of a path, which the verdict already says.
func weakestEvidenceTier(evidence []pivotEdge) string {
	weakest := ""
	for _, edge := range evidence {
		if weakest == "" || tierRank(edge.Tier) > tierRank(weakest) {
			weakest = edge.Tier
		}
	}
	return weakest
}

// incompleteAnalysisFiles collects every file the parser could not fully read,
// mapped to the reason it gave. Both header channels feed it: PartialFailures
// names files that failed outright, Warnings names files that parsed with a
// caveat. Pivot copied both into its response before this change and consulted
// neither, which is what let a file with a broken parse be reported Safe.
func incompleteAnalysisFiles(header sem.SnapshotHeader) map[string]string {
	incomplete := make(map[string]string)
	for _, failure := range header.PartialFailures {
		if failure.FilePath == "" {
			continue
		}
		incomplete[failure.FilePath] = failure.Code
	}
	for _, warning := range header.Warnings {
		if warning.FilePath == "" {
			continue
		}
		if _, exists := incomplete[warning.FilePath]; !exists {
			incomplete[warning.FilePath] = warning.Code
		}
	}
	return incomplete
}

// pivotEvidenceMix renders the tier totals as one line a reader takes in before
// the report itself: how much of what follows the graph proved, and how much it
// inferred. Empty when nothing carries evidence, which is the case for a run that
// invalidated nothing.
func pivotEvidenceMix(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, tier := range []string{evidenceConfirmed, evidenceHeuristic, evidenceUnverified} {
		if counts[tier] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[tier], tier))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	suffix := ""
	if counts[evidenceHeuristic]+counts[evidenceUnverified] > 0 {
		suffix = " — verify the non-confirmed before acting"
	}
	return strings.Join(parts, ", ") + suffix
}
