package cli

// --format plan emits a work order, not a graph dump.
//
// The text report is written for a person deciding what to do. The JSON report is
// the classification in full, which is the right thing to archive and the wrong
// thing to hand an agent: it says what every file is, not what anyone should do
// about it, and an agent handed a classification will invent the plan itself —
// differently each time, and with no way to tell afterwards what it was supposed
// to have done.
//
// So a plan states the work. Every item names one file, one action, the priority
// that orders it against the others, the evidence that justifies it, the
// instructions for carrying it out, and the criteria that decide when it is done.
// The plan also states what it cannot see, because an agent that trusts a SAFE
// verdict as proof of absence will eventually ship a break — and it states what
// the agent is not allowed to claim, because the most likely failure of an agent
// executing this plan is not a bad edit, it is a confident report of success
// nobody ran a command to check.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const pivotPlanSchema = "pivot-plan/v1"

// Actions, one per verdict. The mapping is total: every classified file gets an
// action, including the ones whose action is to be left alone.
const (
	actionReplaceOrRemove    = "REPLACE_OR_REMOVE"
	actionVerifyAfterRootFix = "VERIFY_AFTER_ROOT_FIX"
	actionNoAction           = "NO_ACTION"
	actionManualReview       = "MANUAL_REVIEW"
)

// Priorities. P0 is the only tier that can be worked immediately: everything else
// is either downstream of a P0 fix or is not a change at all.
const (
	priorityP0 = "P0"
	priorityP1 = "P1"
	priorityP2 = "P2"
)

type pivotPlan struct {
	SchemaVersion string `json:"schema_version"`
	// PlanID is derived from the inputs that determine the plan, so the same repo
	// state and the same constraint produce the same id. A random id would make
	// two runs look like two plans.
	PlanID     string              `json:"plan_id"`
	GraphTool  pivotPlanTool       `json:"graph_tool"`
	Repo       string              `json:"repo"`
	HeadCommit string              `json:"head_commit"`
	Constraint pivotPlanConstraint `json:"constraint"`
	Counts     pivotPlanCounts     `json:"counts"`
	// KnownBlindSpots is stated up front rather than footnoted. An agent reading
	// this plan is deciding how much to trust it, and it should decide that before
	// it reads the work items, not after.
	KnownBlindSpots   []pivotBlindSpot       `json:"known_blind_spots"`
	WorkItems         []pivotWorkItem        `json:"work_items"`
	Validation        pivotPlanValidation    `json:"validation"`
	ExecutionContract pivotExecutionContract `json:"execution_contract"`
}

type pivotPlanTool struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Provider      string `json:"provider"`
	SchemaVersion string `json:"graph_schema_version"`
	// Analysis records how the graph was produced, because a plan built from a
	// shallow profile or a dirty worktree deserves less trust than one built from
	// a full parse of a committed tree.
	Depth         int  `json:"depth"`
	ExcludeTests  bool `json:"exclude_tests"`
	IndexCacheHit bool `json:"index_cache_hit"`
}

type pivotPlanConstraint struct {
	// Prohibited is what the caller supplied. The plan deliberately does not carry
	// the sentence it came from: turning prose into a package name happens outside
	// this binary, and recording a paraphrase here would imply the tool checked it.
	Prohibited  []string `json:"prohibited"`
	Rule        string   `json:"rule"`
	Interpreted string   `json:"interpreted_by"`
}

type pivotPlanCounts struct {
	Invalidated   int `json:"invalidated"`
	AtRisk        int `json:"at_risk"`
	Safe          int `json:"safe"`
	Unreached     int `json:"unreached"`
	Total         int `json:"total"`
	ExcludedTests int `json:"excluded_tests"`

	P0 int `json:"p0"`
	P1 int `json:"p1"`
	P2 int `json:"p2"`

	ByRole map[string]int `json:"by_role,omitempty"`
}

type pivotBlindSpot struct {
	ID          string `json:"id"`
	Statement   string `json:"statement"`
	Implication string `json:"implication"`
}

type pivotWorkItem struct {
	ID       string `json:"id"`
	Priority string `json:"priority"`
	Action   string `json:"action"`
	Verdict  string `json:"verdict"`
	Role     string `json:"role"`
	Path     string `json:"path"`
	Package  string `json:"package"`
	// Distance is hops from the root invalidated file; 0 for a direct violation and
	// -1 for a file that no invalidated code reaches.
	Distance        int         `json:"distance"`
	RootInvalidated string      `json:"root_invalidated,omitempty"`
	Evidence        []pivotEdge `json:"evidence"`
	// InCheckpoint marks work in a file the supplied checkpoint's session wrote.
	InCheckpoint       bool     `json:"in_checkpoint,omitempty"`
	AgentInstructions  []string `json:"agent_instructions"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

type pivotPlanValidation struct {
	// RequiredCommands is the whole point of collapsing test fallout by package:
	// the per-package commands land here, where an agent can run them.
	RequiredCommands []string `json:"required_commands"`
	Notes            []string `json:"notes,omitempty"`
}

type pivotExecutionContract struct {
	MustNotClaim []string `json:"must_not_claim"`
	MustDo       []string `json:"must_do,omitempty"`
}

// buildPivotPlan turns a finished classification into the work order. It adds no
// analysis of its own: every field traces to a verdict, a role, or an edge that
// the classifier already produced and printed.
func buildPivotPlan(response pivotResponse, flags pivotFlags) pivotPlan {
	constraint := strings.Join(response.Prohibited, ", ")

	plan := pivotPlan{
		SchemaVersion: pivotPlanSchema,
		PlanID:        pivotPlanID(response, flags),
		GraphTool: pivotPlanTool{
			Name:          "entire-graph pivot",
			Version:       response.Version,
			Provider:      response.Provider,
			SchemaVersion: response.SchemaVersion,
			Depth:         response.MaxDepth,
			ExcludeTests:  response.ExcludeTests,
			IndexCacheHit: response.IndexCacheHit,
		},
		Repo:       response.Repo,
		HeadCommit: response.Commit,
		Constraint: pivotPlanConstraint{
			Prohibited:  response.Prohibited,
			Rule:        fmt.Sprintf("no file may depend on %s", constraint),
			Interpreted: "caller: --dependency takes a package name, this tool does not read prose",
		},
		KnownBlindSpots: pivotKnownBlindSpots(constraint),
	}

	for _, file := range response.Invalidated {
		plan.WorkItems = append(plan.WorkItems, pivotWorkItemFor(file, actionReplaceOrRemove, constraint))
	}
	for _, file := range response.AtRisk {
		plan.WorkItems = append(plan.WorkItems, pivotWorkItemFor(file, actionVerifyAfterRootFix, constraint))
	}
	for _, file := range response.Safe {
		plan.WorkItems = append(plan.WorkItems, pivotWorkItemFor(file, actionNoAction, constraint))
	}
	for _, filePath := range response.Unreached {
		unreached := pivotFile{
			Path:     filePath,
			Verdict:  "UNREACHED",
			Distance: -1,
			Role:     classifyPivotRole(filePath, flags.RepoRoot),
			Why:      response.UnreachedReason,
		}
		plan.WorkItems = append(plan.WorkItems, pivotWorkItemFor(unreached, actionManualReview, constraint))
	}

	// Ordered the way it should be executed, then given stable ids. Ids are assigned
	// after sorting so that reading the plan top to bottom is reading it in order.
	sort.SliceStable(plan.WorkItems, func(a, b int) bool {
		left, right := plan.WorkItems[a], plan.WorkItems[b]
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		if left.Distance != right.Distance {
			return pivotDistanceRank(left.Distance) < pivotDistanceRank(right.Distance)
		}
		if roleRank(left.Role) != roleRank(right.Role) {
			return roleRank(left.Role) < roleRank(right.Role)
		}
		return left.Path < right.Path
	})
	for i := range plan.WorkItems {
		plan.WorkItems[i].ID = fmt.Sprintf("WI-%04d", i+1)
	}

	plan.Counts = pivotPlanCounts{
		Invalidated:   response.Counts.Invalidated,
		AtRisk:        response.Counts.AtRisk,
		Safe:          response.Counts.Safe,
		Unreached:     response.Counts.Unreached,
		Total:         response.Counts.Total,
		ExcludedTests: response.Counts.ExcludedTests,
		ByRole:        response.RoleCounts,
	}
	for _, item := range plan.WorkItems {
		switch item.Priority {
		case priorityP0:
			plan.Counts.P0++
		case priorityP1:
			plan.Counts.P1++
		default:
			plan.Counts.P2++
		}
	}

	plan.Validation = pivotPlanValidation{
		RequiredCommands: pivotRequiredCommands(response, flags),
		Notes: []string{
			"Run every required command. A plan is complete when the commands pass, not when the edits look right.",
			"Re-running the pivot command is what proves the constraint is satisfied: the invalidated count must reach zero.",
		},
	}
	plan.ExecutionContract = pivotExecutionContract{
		MustNotClaim: pivotMustNotClaim(constraint, response),
		MustDo: []string{
			"Work P0 items before P1: an at-risk file cannot be verified until the root it depends on is fixed.",
			"Cite the work item id in each commit so the plan and the history stay linked.",
			"Re-run the pivot command after the edits and attach the new counts to the report.",
		},
	}
	return plan
}

// pivotDistanceRank sorts "no distance" (-1) last rather than first, so unreachable
// and safe files do not lead a list ordered by proximity to the problem.
func pivotDistanceRank(distance int) int {
	if distance < 0 {
		return 1 << 30
	}
	return distance
}

func pivotWorkItemFor(file pivotFile, action, constraint string) pivotWorkItem {
	item := pivotWorkItem{
		Priority:        pivotPriorityFor(file, action),
		Action:          action,
		Verdict:         file.Verdict,
		Role:            file.Role,
		Path:            file.Path,
		Package:         pivotPackageOf(file.Path),
		Distance:        file.Distance,
		RootInvalidated: file.RootInvalidated,
		Evidence:        file.Evidence,
		InCheckpoint:    file.InCheckpoint,
	}
	if item.Evidence == nil {
		item.Evidence = []pivotEdge{}
	}
	item.AgentInstructions = pivotAgentInstructions(file, action, constraint)
	item.AcceptanceCriteria = pivotAcceptanceCriteria(file, action, constraint)
	return item
}

// pivotPriorityFor orders the work by what blocks what.
//
// P0 is a direct violation in code that ships: nothing downstream can be verified
// until it is gone. P1 is everything that is real work but waits — a violation in a
// test, or shipped code standing on an invalidated foundation. P2 is work that is
// covered by running a package, or is not a change at all.
func pivotPriorityFor(file pivotFile, action string) string {
	switch action {
	case actionReplaceOrRemove:
		if isTestRole(file.Role) {
			return priorityP1
		}
		return priorityP0
	case actionVerifyAfterRootFix:
		if isTestRole(file.Role) {
			return priorityP2
		}
		return priorityP1
	case actionManualReview:
		return priorityP2
	default:
		return priorityP2
	}
}

func pivotAgentInstructions(file pivotFile, action, constraint string) []string {
	switch action {
	case actionReplaceOrRemove:
		instructions := []string{
			fmt.Sprintf("Open %s and read the code around each evidence line before editing.", file.Path),
			fmt.Sprintf("Remove the dependency on %s, or replace it with an approved equivalent that provides the same behaviour.", constraint),
			"Do not defeat the check by aliasing the import or moving the call behind a wrapper in the same repository: the dependency would still be present, and the next run would report the wrapper instead.",
		}
		if file.Role == roleGenerated {
			instructions = append(instructions,
				"This file is generated. Change the generator or its template and regenerate; an edit here is overwritten on the next run.")
		}
		if file.Role == roleVendor {
			instructions = append(instructions,
				"This file is vendored. Do not edit it in place — change or drop the dependency that vendors it.")
		}
		if isTestRole(file.Role) {
			instructions = append(instructions,
				"This is test code, so the fix may be to drop the test's use of the dependency rather than to rewrite the behaviour under test.")
		}
		return instructions
	case actionVerifyAfterRootFix:
		root := file.RootInvalidated
		if root == "" {
			root = "the invalidated file it depends on"
		}
		return []string{
			fmt.Sprintf("Do not edit %s first. It breaks no rule on its own; it is here because it depends on %s.", file.Path, root),
			fmt.Sprintf("After %s is fixed, re-read this file's use of it and adjust only what the fix actually changed.", root),
			"If the root fix kept the same interface, the correct outcome for this file is no diff at all.",
		}
	case actionManualReview:
		return []string{
			fmt.Sprintf("The graph parsed %s for structure but not for call or import relations, so it holds no verdict.", file.Path),
			fmt.Sprintf("Read it by hand and decide whether it depends on %s.", constraint),
			"Record the outcome; an unreviewed file in this list is an open question, not a pass.",
		}
	default:
		return []string{
			fmt.Sprintf("Leave %s unchanged. No dependency path from it reaches invalidated code in this snapshot.", file.Path),
			"It is listed so the plan is complete: a file that was analysed and cleared is a different thing from a file nobody looked at.",
		}
	}
}

func pivotAcceptanceCriteria(file pivotFile, action, constraint string) []string {
	pkg := pivotPackageOf(file.Path)
	testCommand := pivotPackageCommand(pkg, strings.HasSuffix(file.Path, ".go"))
	switch action {
	case actionReplaceOrRemove:
		return []string{
			fmt.Sprintf("A re-run of the pivot command no longer lists %s as INVALIDATED.", file.Path),
			"`go build ./...` succeeds.",
			fmt.Sprintf("`%s` passes.", testCommand),
		}
	case actionVerifyAfterRootFix:
		return []string{
			fmt.Sprintf("`%s` passes after the root fix has landed.", testCommand),
			fmt.Sprintf("A re-run of the pivot command reports %s as SAFE, or the remaining edge is explained in the report.", file.Path),
		}
	case actionManualReview:
		return []string{
			fmt.Sprintf("A human or agent has recorded an explicit verdict for %s against %s.", file.Path, constraint),
			"The verdict names what was read to reach it.",
		}
	default:
		return []string{
			fmt.Sprintf("%s is unchanged in the final diff.", file.Path),
		}
	}
}

// pivotKnownBlindSpots is the plan telling the truth about itself. Each entry pairs
// what the analysis cannot see with what that means for the agent reading it,
// because a limitation with no stated consequence gets skimmed.
func pivotKnownBlindSpots(constraint string) []pivotBlindSpot {
	return []pivotBlindSpot{
		{
			ID:          "interface-dispatch",
			Statement:   "A call made through an interface is resolved to the interface, not to the implementation that runs.",
			Implication: "A file whose only route to the prohibited dependency runs through an interface can be reported SAFE. Check implementations of any interface the invalidated files satisfy.",
		},
		{
			ID:          "reflection",
			Statement:   "Reflective and dynamically constructed calls leave no edge in a static parse.",
			Implication: fmt.Sprintf("Code that reaches %s by name at runtime is invisible here. Grep for the string form of the dependency as well.", constraint),
		},
		{
			ID:          "generated-code",
			Statement:   "Generated files are classified from their committed content, and the generator that produced them is not analysed.",
			Implication: "A generator that emits the prohibited dependency will reintroduce it after any hand fix. Fix the generator, then regenerate.",
		},
		{
			ID:          "safe-is-not-proof",
			Statement:   "SAFE means no path was found in this snapshot. It is not proof of absence.",
			Implication: "Do not report the constraint satisfied on the strength of a SAFE verdict alone. Satisfaction is demonstrated by the validation commands, not by this classification.",
		},
		{
			ID:          "unreached-languages",
			Statement:   "Files in inventory-only languages are parsed for structure but not for relations, so they carry no verdict at all.",
			Implication: "Every UNREACHED item is an open question. Treating the SAFE count as the whole remainder undercounts the work.",
		},
		{
			ID:          "name-matching",
			Statement:   "The prohibited dependency is matched on the import path as written, exactly or as a path prefix.",
			Implication: "A dependency reached under an alias, a re-export, or a differently spelled module path is not matched. Confirm the name is the one this repository actually writes.",
		},
	}
}

// pivotRequiredCommands is where the package collapse becomes executable. The test
// fallout that the text report reduced to one line per package is exactly the set
// of commands that has to pass, so the plan carries the commands rather than the
// file list.
func pivotRequiredCommands(response pivotResponse, flags pivotFlags) []string {
	affected := make([]pivotFile, 0, len(response.Invalidated)+len(response.AtRisk))
	affected = append(affected, response.Invalidated...)
	affected = append(affected, response.AtRisk...)

	commands := []string{"go build ./..."}
	for _, group := range collapsePivotPackages(affected) {
		if strings.HasPrefix(group.Command, "go test") {
			commands = append(commands, group.Command)
		}
	}
	commands = append(commands, pivotRerunCommand(response, flags))
	return commands
}

// pivotRerunCommand reconstructs the invocation that produced this plan, so the
// agent can prove the counts moved rather than assert it.
func pivotRerunCommand(response pivotResponse, flags pivotFlags) string {
	var builder strings.Builder
	builder.WriteString("entire graph pivot --repo .")
	for _, dependency := range response.Prohibited {
		builder.WriteString(" --dependency ")
		builder.WriteString(dependency)
	}
	if response.ExcludeTests {
		builder.WriteString(" --exclude-tests")
	}
	if response.MaxDepth != pivotMaxDepth {
		fmt.Fprintf(&builder, " --depth %d", response.MaxDepth)
	}
	if flags.Worktree {
		builder.WriteString(" --worktree")
	}
	return builder.String()
}

// pivotMustNotClaim names the failures that are likely enough to be worth forbidding
// by name. The dangerous outcome of handing an agent a plan is not a bad edit — a
// bad edit fails a test. It is a confident report of success that nobody checked.
func pivotMustNotClaim(constraint string, response pivotResponse) []string {
	claims := []string{
		fmt.Sprintf("Must not claim %s has been removed without re-running the pivot command and showing the invalidated count at zero.", constraint),
		"Must not claim any test passes without having executed it and read the result.",
		"Must not present a SAFE verdict as proof that no dependency path exists; it means none was found in this snapshot.",
		"Must not count an UNREACHED file as resolved. It was never analysed for relations.",
		"Must not report the plan complete while any P0 item is open, regardless of how many P1 and P2 items were closed.",
		"Must not silently widen the plan: a file that is not a work item here has not been assessed for this change.",
	}
	if response.RoleCounts[roleGenerated] > 0 {
		claims = append(claims,
			"Must not claim a GENERATED file is fixed by an edit to the file itself; the generator decides its next content.")
	}
	if response.RoleCounts[roleVendor] > 0 {
		claims = append(claims,
			"Must not claim a VENDOR file is fixed by an in-place edit; vendored code is replaced by changing the dependency.")
	}
	return claims
}

// pivotPlanID is a digest of everything that determines the plan's content. Same
// tree, same constraint, same flags produces the same id, so two runs of the same
// question are recognisably the same plan rather than two.
func pivotPlanID(response pivotResponse, flags pivotFlags) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\n%s\n%d\n%t\n%t\n",
		response.Commit, strings.Join(response.Prohibited, ","),
		response.MaxDepth, response.ExcludeTests, flags.Worktree)
	fmt.Fprintf(digest, "%d/%d/%d/%d\n",
		response.Counts.Invalidated, response.Counts.AtRisk,
		response.Counts.Safe, response.Counts.Unreached)
	return "pivot-" + hex.EncodeToString(digest.Sum(nil))[:12]
}
