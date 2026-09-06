package cli

import (
	"strings"
	"testing"
)

// planTestResponse is a finished classification covering every verdict and both
// sides of the role split, so one fixture can exercise the whole action mapping.
func planTestResponse() pivotResponse {
	return pivotResponse{
		SchemaVersion: "1.1",
		Provider:      "entire-graph",
		Version:       "test",
		Repo:          "/tmp/entire-graph",
		Commit:        "934d180dabcdef",
		Prohibited:    []string{"os/exec"},
		MaxDepth:      2,
		Invalidated: []pivotFile{
			{Path: "internal/gitutil/git.go", Verdict: verdictInvalidated, Role: roleProduction, Distance: 0,
				RootInvalidated: "internal/gitutil/git.go",
				Evidence:        []pivotEdge{{Relation: "IMPORTS", Target: "os/exec", Confidence: 0.8, Line: 12}}},
			{Path: "internal/sem/a_test.go", Verdict: verdictInvalidated, Role: roleTest, Distance: 0,
				RootInvalidated: "internal/sem/a_test.go"},
		},
		AtRisk: []pivotFile{
			{Path: "internal/cli/verify.go", Verdict: verdictAtRisk, Role: roleProduction, Distance: 1,
				RootInvalidated: "internal/gitutil/git.go"},
			{Path: "internal/sem/b_test.go", Verdict: verdictAtRisk, Role: roleTest, Distance: 2,
				RootInvalidated: "internal/gitutil/git.go"},
		},
		Safe:            []pivotFile{{Path: "internal/cli/help.go", Verdict: verdictSafe, Role: roleProduction, Distance: -1}},
		Unreached:       []string{"docs/design.md"},
		UnreachedReason: "inventory-only language",
		RoleCounts:      map[string]int{roleProduction: 3, roleTest: 2},
		Counts:          pivotCounts{Invalidated: 2, AtRisk: 2, Safe: 1, Unreached: 1, Total: 6},
	}
}

// TestBuildPivotPlanMapsEveryVerdictToAnAction is the contract the plan rests on:
// the mapping is total. A verdict with no action would leave a file in the report
// but out of the work order, which is the silent gap the plan exists to close.
func TestBuildPivotPlanMapsEveryVerdictToAnAction(t *testing.T) {
	plan := buildPivotPlan(planTestResponse(), pivotFlags{Depth: 2})

	if plan.SchemaVersion != pivotPlanSchema {
		t.Errorf("schema_version = %q, want %q", plan.SchemaVersion, pivotPlanSchema)
	}
	if plan.HeadCommit != "934d180dabcdef" {
		t.Errorf("head_commit = %q", plan.HeadCommit)
	}
	if plan.GraphTool.Version != "test" || plan.GraphTool.SchemaVersion != "1.1" {
		t.Errorf("graph tool provenance missing: %+v", plan.GraphTool)
	}
	if len(plan.WorkItems) != 6 {
		t.Fatalf("want one work item per classified file (6), got %d", len(plan.WorkItems))
	}

	byPath := make(map[string]pivotWorkItem, len(plan.WorkItems))
	for _, item := range plan.WorkItems {
		byPath[item.Path] = item
	}
	for path, want := range map[string]struct{ action, priority string }{
		"internal/gitutil/git.go": {actionReplaceOrRemove, priorityP0},
		"internal/sem/a_test.go":  {actionReplaceOrRemove, priorityP1},
		"internal/cli/verify.go":  {actionVerifyAfterRootFix, priorityP1},
		"internal/sem/b_test.go":  {actionVerifyAfterRootFix, priorityP2},
		"internal/cli/help.go":    {actionNoAction, priorityP2},
		"docs/design.md":          {actionManualReview, priorityP2},
	} {
		item, ok := byPath[path]
		if !ok {
			t.Errorf("no work item for %s", path)
			continue
		}
		if item.Action != want.action {
			t.Errorf("%s action = %q, want %q", path, item.Action, want.action)
		}
		if item.Priority != want.priority {
			t.Errorf("%s priority = %q, want %q", path, item.Priority, want.priority)
		}
		if len(item.AgentInstructions) == 0 || len(item.AcceptanceCriteria) == 0 {
			t.Errorf("%s has no instructions or criteria; a work item without either is not actionable", path)
		}
	}
	if plan.Counts.P0 != 1 || plan.Counts.P1 != 2 || plan.Counts.P2 != 3 {
		t.Errorf("priority counts = %d/%d/%d, want 1/2/3", plan.Counts.P0, plan.Counts.P1, plan.Counts.P2)
	}
}

// TestBuildPivotPlanOrdersWorkByWhatBlocksWhat checks that reading the plan top to
// bottom is reading it in execution order, and that the ids follow that order.
func TestBuildPivotPlanOrdersWorkByWhatBlocksWhat(t *testing.T) {
	plan := buildPivotPlan(planTestResponse(), pivotFlags{Depth: 2})
	if plan.WorkItems[0].ID != "WI-0001" || plan.WorkItems[0].Priority != priorityP0 {
		t.Fatalf("first item should be WI-0001 at P0, got %s at %s", plan.WorkItems[0].ID, plan.WorkItems[0].Priority)
	}
	previous := ""
	for _, item := range plan.WorkItems {
		if previous != "" && item.Priority < previous {
			t.Fatalf("work items out of priority order at %s (%s after %s)", item.ID, item.Priority, previous)
		}
		previous = item.Priority
	}
	// The at-risk item must point at the root that has to be fixed first, not at
	// the neighbour it happened to be reached through.
	for _, item := range plan.WorkItems {
		if item.Path == "internal/sem/b_test.go" && item.RootInvalidated != "internal/gitutil/git.go" {
			t.Errorf("two-hop item lost its root: %q", item.RootInvalidated)
		}
	}
}

// TestBuildPivotPlanStatesItsOwnBlindSpots guards the honesty of the plan. These
// are the claims an agent would otherwise make on its own, so their absence is a
// correctness problem rather than a documentation one.
func TestBuildPivotPlanStatesItsOwnBlindSpots(t *testing.T) {
	plan := buildPivotPlan(planTestResponse(), pivotFlags{Depth: 2})
	required := []string{"interface-dispatch", "reflection", "generated-code", "safe-is-not-proof"}
	present := make(map[string]bool, len(plan.KnownBlindSpots))
	for _, spot := range plan.KnownBlindSpots {
		present[spot.ID] = true
		if spot.Statement == "" || spot.Implication == "" {
			t.Errorf("blind spot %q states a limitation with no consequence", spot.ID)
		}
	}
	for _, id := range required {
		if !present[id] {
			t.Errorf("plan does not declare blind spot %q", id)
		}
	}
	joined := strings.Join(plan.ExecutionContract.MustNotClaim, "\n")
	for _, want := range []string{"without re-running the pivot command", "without having executed it", "proof that no dependency path exists"} {
		if !strings.Contains(joined, want) {
			t.Errorf("execution contract missing a clause about %q:\n%s", want, joined)
		}
	}
}

// TestPivotRequiredCommandsAreRunnable is where the package collapse pays off: the
// test fallout the report reduced to a line per package has to arrive here as
// commands, plus the re-run that proves the constraint is actually satisfied.
func TestPivotRequiredCommandsAreRunnable(t *testing.T) {
	plan := buildPivotPlan(planTestResponse(), pivotFlags{Depth: 2})
	commands := plan.Validation.RequiredCommands
	if len(commands) == 0 || commands[0] != "go build ./..." {
		t.Fatalf("first required command should be the build, got %v", commands)
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{"go test ./internal/gitutil", "go test ./internal/cli", "go test ./internal/sem"} {
		if !strings.Contains(joined, want) {
			t.Errorf("required commands missing %q:\n%s", want, joined)
		}
	}
	last := commands[len(commands)-1]
	if !strings.HasPrefix(last, "entire graph pivot") || !strings.Contains(last, "--dependency os/exec") {
		t.Errorf("plan must end by re-running itself, got %q", last)
	}
}

// TestPivotRerunCommandReproducesTheInvocation matters because the re-run is the
// evidence the work is done. A command that does not match the run that produced
// the plan would compare two different questions.
func TestPivotRerunCommandReproducesTheInvocation(t *testing.T) {
	response := planTestResponse()
	response.ExcludeTests = true
	response.MaxDepth = 1
	got := pivotRerunCommand(response, pivotFlags{})
	want := "entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1"
	if got != want {
		t.Errorf("rerun command = %q, want %q", got, want)
	}
}

// TestPivotPlanIDIsDeterministic keeps two runs of the same question recognisable
// as the same plan, and two different questions distinguishable.
func TestPivotPlanIDIsDeterministic(t *testing.T) {
	response := planTestResponse()
	first := buildPivotPlan(response, pivotFlags{Depth: 2}).PlanID
	second := buildPivotPlan(response, pivotFlags{Depth: 2}).PlanID
	if first != second {
		t.Errorf("same inputs produced two plan ids: %s and %s", first, second)
	}
	if !strings.HasPrefix(first, "pivot-") {
		t.Errorf("plan id %q is not namespaced", first)
	}
	changed := planTestResponse()
	changed.Prohibited = []string{"net/http"}
	if buildPivotPlan(changed, pivotFlags{Depth: 2}).PlanID == first {
		t.Error("a different constraint produced the same plan id")
	}
}

// TestPivotRoleAwareContractClauses checks the contract adapts to what is actually
// in the tree: telling an agent not to hand-edit generated code is noise when the
// repository has none, and a missing safeguard when it does.
func TestPivotRoleAwareContractClauses(t *testing.T) {
	plain := buildPivotPlan(planTestResponse(), pivotFlags{Depth: 2})
	if strings.Contains(strings.Join(plain.ExecutionContract.MustNotClaim, "\n"), "GENERATED") {
		t.Error("contract warns about generated files in a tree that has none")
	}
	withGenerated := planTestResponse()
	withGenerated.RoleCounts = map[string]int{roleGenerated: 1, roleVendor: 2}
	joined := strings.Join(buildPivotPlan(withGenerated, pivotFlags{Depth: 2}).ExecutionContract.MustNotClaim, "\n")
	if !strings.Contains(joined, "GENERATED") || !strings.Contains(joined, "VENDOR") {
		t.Errorf("contract missing role-specific clauses:\n%s", joined)
	}
}

// TestParsePivotFlagsAcceptsPlanFormat is the wiring check.
func TestParsePivotFlagsAcceptsPlanFormat(t *testing.T) {
	flags, err := parsePivotFlags([]string{"--dependency", "os/exec", "--format", "plan"})
	if err != nil {
		t.Fatalf("plan format rejected: %v", err)
	}
	if flags.Format != "plan" {
		t.Errorf("format = %q", flags.Format)
	}
	if _, err := parsePivotFlags([]string{"--dependency", "os/exec", "--format", "yaml"}); err == nil {
		t.Error("unknown format accepted")
	}
}
