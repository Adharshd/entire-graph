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
	Unverified    int `json:"unverified"`
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
	// EvidenceQuality and VerificationRequired are the agent-facing half of the
	// evidence tiers. The text report tags each evidence line for a human; an
	// agent consuming this plan needs the same distinction as a field it can
	// branch on, because an agent is the reader most likely to act on a work
	// item without opening the file.
	EvidenceQuality      string `json:"evidence_quality,omitempty"`
	VerificationRequired bool   `json:"verification_required"`

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
	// Invariants are the machine-checkable form of the execution contract.
	Invariants pivotInvariants `json:"invariants"`
}

// pivotInvariants is must_not_do restated as data. The prose clauses bind an
// agent that chooses to read them; these are assertions a third party evaluates
// against the resulting commit without asking the agent anything.
//
// The split matters because the two failure modes are different. An agent that
// misunderstands the task is helped by prose. An agent that has found the cheapest
// path to a passing number is not, and that is the case the verification loop
// actually hit: `invalidated == 0` is satisfied by deleting the code.
type pivotInvariants struct {
	// FilesMustStillExist are the invalidated production files. Removing one is
	// the deletion attack in its simplest form -- the violation goes away with
	// the code, and every count in the report improves.
	FilesMustStillExist []string `json:"files_must_still_exist"`
	// PathsMustNotChange are the test files this run classified. Weakening a
	// check is how a required command starts passing without the work being done.
	PathsMustNotChange []string `json:"paths_must_not_change"`
	// MaxNetLinesDeleted is a deletion budget: insertions minus deletions across
	// the whole change, floored at zero. See pivotDeletionBudget for how the
	// number is derived; Basis carries that derivation into the JSON so a
	// consumer is not asked to trust a bare constant.
	MaxNetLinesDeleted      int    `json:"max_net_lines_deleted"`
	MaxNetLinesDeletedBasis string `json:"max_net_lines_deleted_basis"`
	// FlagsMustMatch pins the question. A re-run with a different constraint,
	// depth or test-exclusion produces a number that is not comparable to the one
	// in this plan, and narrowing the question is easier than doing the work.
	FlagsMustMatch pivotInvariantFlags `json:"flags_must_match"`
	// ExpectedAfter is what a satisfied constraint looks like on a re-run.
	ExpectedAfter pivotExpectedAfter `json:"expected_after"`
	// NotMechanicallyCheckable is the honest half, and the reason a clean result
	// from these checks is PARTIAL rather than PASS. Everything a deterministic
	// verifier cannot decide is named here rather than left to look checked.
	NotMechanicallyCheckable []string `json:"not_mechanically_checkable"`
}

type pivotInvariantFlags struct {
	Dependency   []string `json:"dependency"`
	Depth        int      `json:"depth"`
	ExcludeTests bool     `json:"exclude_tests"`
}

type pivotExpectedAfter struct {
	Invalidated int `json:"invalidated"`
	// UnverifiedMustNotIncrease guards the other direction: silencing a parser
	// diagnostic would move a file out of UNVERIFIED without anyone reading it.
	UnverifiedMustNotIncrease int `json:"unverified_must_not_exceed"`
}

type pivotExecutionContract struct {
	MustNotClaim []string `json:"must_not_claim"`
	// MustNotDo are anti-trivialisation invariants: the ways an agent can drive
	// the invalidated count to zero without satisfying the constraint. They are
	// separate from MustNotClaim because they bind the WORK, not the report --
	// deleting the file is not a false claim, it is a false fix.
	MustNotDo []string `json:"must_not_do,omitempty"`
	MustDo    []string `json:"must_do,omitempty"`
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
	for _, file := range response.Unverified {
		plan.WorkItems = append(plan.WorkItems, pivotWorkItemFor(file, actionManualReview, constraint))
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
		Unverified:    response.Counts.Unverified,
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
		Invariants:       pivotBuildInvariants(response, flags),
		Notes: []string{
			"Run every required command. A plan is complete when the commands pass, not when the edits look right.",
			"Re-running the pivot command is what proves the constraint is satisfied: the invalidated count must reach zero.",
			"The count alone is not sufficient. Attach the diff: invalidated == 0 is satisfied by deleting the code as well as by fixing it.",
			"Re-run from a clean checkout of the resulting commit, not from the working tree the edits were made in. A verifier that shares the agent's uncommitted state is not verifying the commit.",
			"A required command that could not be executed -- missing tool, timeout, network failure -- is CANNOT_VERIFY, not a pass. Report it as unverified and say which command it was.",
		},
	}
	plan.ExecutionContract = pivotExecutionContract{
		MustNotClaim: pivotMustNotClaim(constraint, response),
		MustNotDo:    pivotMustNotDo(constraint, response),
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
		EvidenceQuality: file.EvidenceQuality,
		// A NO_ACTION item on a clean parse needs no verification: the plan is
		// telling the agent to do nothing, and doing nothing cannot be wrong in
		// a way source inspection would catch. Everything else that rests on
		// inference does.
		VerificationRequired: file.VerificationRequired,
		Distance:             file.Distance,
		RootInvalidated:      file.RootInvalidated,
		Evidence:             file.Evidence,
		InCheckpoint:         file.InCheckpoint,
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
			ID:          "heuristic-edges",
			Statement:   "Not every edge in this plan was resolved to a declaration. Edges tagged HEURISTIC were inferred from a name, a package, or an inferred receiver type; the graph records that difference and each work item carries it as evidence_quality.",
			Implication: "A work item whose evidence is HEURISTIC may point at the wrong symbol. Open the cited line and confirm the relation before editing, and never report the item as done on the strength of the edge alone.",
		},
		{
			ID:          "unverified-parse",
			Statement:   "Files the parser could not fully read are reported UNVERIFIED rather than SAFE. No dependency path was found in them, but no dependency path could have been found in them.",
			Implication: "Treat an UNVERIFIED file as unexamined, not as cleared. It needs a source read or a test, and it must not be counted towards the constraint being satisfied.",
		},
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
// pivotMustNotDo names the ways this specific plan can be satisfied without being
// done. Each one is an invariant an agent could otherwise break while every number
// in the report improves.
//
// The distinction from must_not_claim is worth keeping: deleting a file is not a
// false claim about the work, it is false work. A contract that only constrains
// the report leaves the cheapest wrong answer available.
//
// This is deliberately not a full verification contract. The research this comes
// from proposes a declarative schema of named checks; most of what it asks a
// developer to configure is something the tool already knows, so the checks stay
// tool-owned and only the invariants are stated here.
// observedNetLinesPerReplacedFile is the one real measurement this budget rests
// on. The verification loop replaced `bytes` with `strings` in one file of
// gorilla/mux and produced `1 file changed, 7 insertions(+), 8 deletions(-)` --
// a net of one line removed. A genuine dependency replacement is close to
// line-neutral, because the code that used the dependency is rewritten rather
// than removed.
const observedNetLinesPerReplacedFile = 1

// deletionBudgetHeadroom multiplies that observation to leave room for a real
// refactor -- extracting a helper, dropping a now-redundant wrapper -- without
// leaving room for a deletion. Even the smallest Go source file in this
// repository is tens of lines, so removing one outright breaks the budget
// immediately at this multiple, while a legitimate rewrite has an order of
// magnitude more slack than the mux fix needed.
const deletionBudgetHeadroom = 10

// pivotDeletionBudget scales the observed cost of a real fix by the number of
// files that actually have to be fixed. It is a budget on the NET (deletions
// minus insertions) across the whole change, so a rewrite that removes a hundred
// lines and adds a hundred spends nothing.
//
// The floor exists because a plan with no invalidated production files still
// wants a non-zero budget: a run whose violations are all in test files can
// legitimately change a few lines, and a budget of zero would fail it for that.
func pivotDeletionBudget(productionViolations int) int {
	budget := productionViolations * observedNetLinesPerReplacedFile * deletionBudgetHeadroom
	if budget < deletionBudgetHeadroom {
		return deletionBudgetHeadroom
	}
	return budget
}

// pivotBuildInvariants restates the contract as assertions. Everything here is
// derived from verdicts the classifier already produced -- it adds no analysis,
// exactly as the rest of the plan does not.
func pivotBuildInvariants(response pivotResponse, flags pivotFlags) pivotInvariants {
	invariants := pivotInvariants{
		FilesMustStillExist: []string{},
		PathsMustNotChange:  []string{},
		FlagsMustMatch: pivotInvariantFlags{
			Dependency:   response.Prohibited,
			Depth:        response.MaxDepth,
			ExcludeTests: response.ExcludeTests,
		},
		ExpectedAfter: pivotExpectedAfter{
			Invalidated:               0,
			UnverifiedMustNotIncrease: response.Counts.Unverified,
		},
	}

	productionViolations := 0
	for _, file := range response.Invalidated {
		if isTestRole(file.Role) {
			continue
		}
		// A generated or vendored file is not edited in place, so requiring it to
		// survive would be asserting something the plan itself tells the agent not
		// to do.
		if file.Role == roleGenerated || file.Role == roleVendor {
			continue
		}
		productionViolations++
		invariants.FilesMustStillExist = append(invariants.FilesMustStillExist, file.Path)
	}

	for _, group := range [][]pivotFile{response.Invalidated, response.AtRisk, response.Safe, response.Unverified} {
		for _, file := range group {
			if isTestRole(file.Role) {
				invariants.PathsMustNotChange = append(invariants.PathsMustNotChange, file.Path)
			}
		}
	}
	sort.Strings(invariants.FilesMustStillExist)
	sort.Strings(invariants.PathsMustNotChange)

	invariants.MaxNetLinesDeleted = pivotDeletionBudget(productionViolations)
	invariants.MaxNetLinesDeletedBasis = fmt.Sprintf(
		"%d production violation(s) x %d net line(s) observed for a real dependency replacement (gorilla/mux: 1 file, +7/-8) x %dx headroom, floored at %d",
		productionViolations, observedNetLinesPerReplacedFile, deletionBudgetHeadroom, deletionBudgetHeadroom)

	invariants.NotMechanicallyCheckable = pivotUncheckable(response, flags)
	return invariants
}

// pivotUncheckable names what a deterministic verifier cannot decide. It is never
// empty, and that is deliberate: a checker whose uncheckable list came back empty
// would be claiming it had covered everything, which no static check over a diff
// can do. This list is what turns an otherwise clean result into PARTIAL.
func pivotUncheckable(response pivotResponse, flags pivotFlags) []string {
	items := []string{
		"Whether the replacement preserves behaviour. A diff within budget and a green test run are consistent with a subtly wrong rewrite; only a test that exercises the changed path can speak to this.",
		"Whether the required commands actually cover the changed code. Passing tests prove the tests passed, not that they touched the edit.",
		"Whether a SAFE verdict reflects the absence of a path or only the absence of an edge. Calls through interfaces, reflection and generated code leave no edge to follow.",
	}
	if heuristic := response.EvidenceCounts[evidenceHeuristic]; heuristic > 0 {
		items = append(items, fmt.Sprintf(
			"Whether the %d file(s) resting on HEURISTIC evidence are correctly classified. Those edges were matched by name or package rather than resolved, and confirming one means reading the cited source line.", heuristic))
	}
	if response.Counts.Unverified > 0 {
		items = append(items, fmt.Sprintf(
			"Whether the %d UNVERIFIED file(s) are affected. The parser could not read them, so neither this plan nor any check over its output can say.", response.Counts.Unverified))
	}
	if flags.ExcludeTests {
		// Said out loud because the invariant silently weakens here: with tests
		// excluded before classification, the plan cannot name the test files that
		// must not change, so an empty paths_must_not_change means "not checked",
		// never "nothing to check".
		items = append(items, "Whether any test was weakened. --exclude-tests dropped test files before classification, so paths_must_not_change is empty for this run: absence of listed test paths is absence of information, not evidence that no test moved.")
	}
	return items
}

func pivotMustNotDo(constraint string, response pivotResponse) []string {
	rules := []string{
		fmt.Sprintf("Must not delete production code to remove %s. A violation count that falls because the code is gone is not the constraint being satisfied; it is the measurement being satisfied. Replace the dependency, keep the behaviour.", constraint),
		"Must not delete, skip, or weaken a test to make a required command pass. If a test now fails for a reason the plan did not anticipate, report it as a finding rather than removing the check that found it.",
		"Must not change the constraint to fit the result: re-run with the same --dependency, --depth and --exclude-tests recorded above, or the before and after counts are not comparable.",
		"Must not narrow the analysed set. Adding an ignore rule, excluding a path, or reducing the profile lowers the count without changing the code.",
	}
	if response.Counts.Unverified > 0 {
		rules = append(rules,
			fmt.Sprintf("Must not resolve any of the %d UNVERIFIED file(s) by suppressing the parser diagnostic. The file is unverified because it could not be read, and silencing the reader does not make it readable.", response.Counts.Unverified))
	}
	// The invariant the mux run checked by hand: one file, +7/-8, no test touched.
	// Naming the shape of an acceptable diff is what makes it checkable by someone
	// other than the agent that produced it.
	rules = append(rules,
		"Must attach `git diff --stat` for the change alongside the re-run counts. A reviewer needs to see that lines were replaced rather than removed, and that no test file was touched to get there.")
	return rules
}

func pivotMustNotClaim(constraint string, response pivotResponse) []string {
	claims := []string{
		fmt.Sprintf("Must not claim %s has been removed without re-running the pivot command and showing the invalidated count at zero.", constraint),
		"Must not claim any test passes without having executed it and read the result.",
		"Must not present a SAFE verdict as proof that no dependency path exists; it means none was found in this snapshot.",
		"Must not count an UNREACHED file as resolved. It was never analysed for relations.",
		// The clause this curveball exists for. An agent that reads an edge as a
		// fact will act on a name match with the same confidence it acts on an
		// import declaration, and report both the same way afterwards.
		"Must not treat a HEURISTIC edge as an established fact. Any work item whose evidence_quality is not CONFIRMED requires the cited source line to be read, or a test to be run, before the item is acted on or reported done.",
		"Must not count an UNVERIFIED file towards the constraint being satisfied. The parser failed on it, so the absence of a path in it is unknown rather than established.",
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
