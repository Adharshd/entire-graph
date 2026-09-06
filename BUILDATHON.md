# PivotMap

> **Where the work is.** All of it is on the branch **`pivot-work-order`**, not on `main`.
> `main` is protected on the Entire mirror and the push to it was rejected, so the repository root
> renders a stale tree with no checkpoint activity. That is a display artefact, not the state of the
> project — confirm with `git ls-remote origin`.
>
> ```
> Branch      pivot-work-order
> Tree        https://entire.io/gh/Adharshd/entire-graph/tree/pivot-work-order
> ```

## Start here

Three layers, shallowest first. Stop at whichever answers your question.

**1 — What it is, in one screen**
[One-sentence summary](#one-sentence-summary) · [Problem](#problem-intended-user-and-why-it-matters) ·
[Why Entire is essential](#selected-entire-track-and-why-entire-is-essential)

**2 — The Noon Curveball response** (the graded work)
[Every requirement, and where it is answered](#every-requirement-and-where-it-is-answered) — start
here if you are scoring it; each row names the file or test that answers it.
Then [what assumption was invalidated](#the-assumption-that-was-invalidated),
[what changed](#what-changed), [why it is safe](#why-the-new-result-is-safe), and the
[run against Kubernetes](#run-against-kubernetes).

**3 — The evidence, reproducible**
[`docs/demo/curveball/`](docs/demo/curveball/) — every graph command, its output, and the plan that
was followed. Items 01-06 ran before the first line of code changed.
[Checkpoint → commit table](#checkpoint-links-and-what-each-checkpoint-proves) maps each milestone to
the commit that carries it.

### If you are an agent reading this

The machine-readable form of this project is the work order, not this document:

```sh
go build -o ./entire-graph ./cmd/entire-graph        # the installed plugin has no pivot subcommand
./entire-graph pivot --repo . --dependency internal/sem --exclude-tests --depth 1 --format plan
```

`pivot-plan/v1` states the work rather than the classification. Read
`known_blind_spots` and `execution_contract.must_not_claim` **before** the work items — they are
placed first because the decision about how far to trust a plan is made before it is read, not after.
Each work item carries `evidence_quality` (`CONFIRMED` / `HEURISTIC` / `UNVERIFIED`) and
`verification_required`. Anything not `CONFIRMED` requires the cited source line to be read, or a
test run, before the item is acted on or reported done.

A captured example is `docs/demo/curveball/14-pivot-on-itself-plan.json`.

---

## One-sentence summary

`entire graph pivot` answers what survives a requirement that changed after the code was already
written: which files break the new rule, which ones are built on the files that break it, and which
ones can be left alone.

## Problem, intended user and why it matters

A constraint arrives after the work is done. A dependency may no longer touch customer data. An
architecture rule now forbids background workers. A vendor is dropped. The code is already written,
and the developer now has to answer three questions before touching anything:

1. Which code actually violates this?
2. What else breaks if I rip that out?
3. What can I safely leave alone?

Today that is manual archaeology. `git log` shows what changed but not why anyone chose it. Grep finds
the banned import but not the three modules quietly built on top of it. So the developer either
over-reworks — treating the whole area as suspect — or under-reworks and ships a break.

The user is a developer or a coding agent that has just been handed a changed requirement and needs a
defensible plan for what to keep, rework, and drop, in an order they can start executing immediately.

## Selected Entire track and why Entire is essential

**Track 2 — Build with Graph Intelligence.**

The two halves of this problem map onto Entire's two primitives, and neither half is answerable alone:

- **Entire Graph** knows what connects to what. Given a prohibited package it can name every file that
  imports it, and every file that depends on those, with a confidence score and a source line per edge.
  It cannot know why any of it was built.
- **Entire Checkpoints** know why. A checkpoint carries the session that produced a commit — the intent,
  the reasoning, what was rejected. It has no idea what depends on the result.

PivotMap is the join. Graph supplies the blast radius; a checkpoint supplies the intent behind the code
inside that radius and flags which of the affected files this session actually wrote. Remove Graph and
there is no propagation, only grep. Remove checkpoints and the report can still classify, but it loses
the "why was this built this way" that makes a rework plan trustworthy.

## Architecture and main workflow

One new command in this fork, in three files, registered in the existing dispatcher in
`internal/cli/root.go`:

- `internal/cli/pivot.go` — flags, graph load, the three classification rules, propagation, JSON.
- `internal/cli/pivot_roles.go` — file-role classification and the grouped text report.
- `internal/cli/pivot_plan.go` — the `pivot-plan/v1` work order.

```
entire graph pivot --dependency <prohibited package> [--checkpoint <id>] [--depth N]
                   [--exclude-tests] [--format text|json|plan]
```

Pipeline:

1. **Load the graph.** `sem.LoadOrBuildProviderSnapshot` — the same call `impact` uses, so pivot reuses
   the plugin's existing parse and cache rather than building a second index.
2. **Rule 1, direct conflict.** Any file with an `IMPORTS`, `CALLS`, or `CONSTRUCTS` edge reaching the
   prohibited dependency is **INVALIDATED**. This is checkpoint-independent by design: code written
   before Entire was ever enabled still violates a new constraint, and a classifier that could only see
   checkpointed files would silently clear all of it.
3. **Rule 2, propagation.** Breadth-first from the invalidated set over dependency edges. Anything
   reached is **AT-RISK**, recorded with its hop distance. Capped at 2 hops by default.
4. **Rule 3, the rest.** No path to anything invalidated means **SAFE** — but only for languages the
   graph parses semantically. Files in inventory-only languages are reported as **UNREACHED**, because
   calling a file safe when the parser never checked it for relations would be asserting an absence
   nobody verified.
5. **Checkpoint overlay (optional).** `sem.AnalyzeCheckpoint` folds in what a session changed and how
   many dependents each change has, and marks affected files that session wrote with `*`.
6. **Role pass.** Every file is classified **PRODUCTION | TEST | TEST_FIXTURE | GENERATED | VENDOR |
   UNKNOWN** from signals that cannot be argued with: a `vendor/` path segment, a
   `testdata|fixtures|mocks` segment, the `_test.go` suffix, or the `Code generated ... DO NOT EDIT.`
   header the generator itself wrote. The role never changes a verdict — it decides how the finished
   report is grouped and ranked.
7. **Render.** `text` for a person, `json` for the full classification, `plan` for an agent.

`--exclude-tests` (the same path predicate `impact` and `neighbors` use) drops test files *before*
classification rather than filtering them out of the finished report. The difference matters for
propagation, not just for the listing: a test file is not a foundation anything ships on, so a
production file whose only route to invalidated code runs through a test file should come back Safe,
not At-Risk. The count of excluded files is printed, because a file that was never classified is not
a file that came back Safe.

Every verdict prints the edge that produced it — relation type, target, confidence, source line — so a
reader can check the claim rather than trust it.

### Three design decisions worth defending

**The classifier is rules, not a model.** No LLM produces a verdict. The interpretive step — turning a
sentence like "we can't send customer data through Library X" into the package name `libraryx` — stays
outside this binary, and `--dependency` takes the result directly. That is not only a design
preference: `entire graph doctor` asserts `no_egress=true`, and a model call inside the plugin would
make the plugin's own diagnostic lie.

**Propagation is capped.** Unbounded transitive closure marks an entire repository At-Risk, which is
technically true and completely useless. Two hops, with the distance reported per file, keeps the
output a shortlist a person can actually work through.

**Roles rank the report; they never filter it.** The first version of this tool had a real usability
bug, and it was not in the classifier. One shared test helper reaching invalidated code dragged its
whole package along, and the report printed all of it — every file, with its own evidence block. The
verdicts were correct and the output was unusable, because 187 file names that all resolve to one
`go test ./internal/sem` bury the eight production files somebody actually has to open. The fix is not
to drop those files: a deleted result is a result nobody can audit. The fix is to rank them. Test
fallout collapses to one line per package carrying the command that checks it, production work stays
listed file by file with its evidence, and the JSON still carries all 636 files with their roles.

## Entire Graph findings and verification

Run against this repository at commit `934d180` — 634 files, of which the role classifier calls 277
PRODUCTION, 266 TEST and 91 TEST_FIXTURE:

| Run | INVALIDATED | AT-RISK | SAFE | UNREACHED | excluded |
|---|---|---|---|---|---|
| `--dependency net/http` | 3 | 78 | 468 | 85 | 0 |
| `--dependency net/http --exclude-tests` | **0** | **0** | 204 | 73 | 357 |
| `--dependency os/exec` | 32 | 287 | 230 | 85 | 0 |
| `--dependency os/exec --exclude-tests` | 8 | 100 | 96 | 73 | 357 |
| `--dependency os/exec --exclude-tests --depth 1` | 8 | 11 | 185 | 73 | 357 |

These were re-measured from a clean detached build at `934d180`, in a scratch worktree, rather than
carried forward from an earlier run.

**Read this table as a frozen snapshot at `934d180`, not as what you will see today.** The counts are
file counts, and later commits in this branch added files — `pivot_plan.go`, its test, the demo
evidence under `docs/demo/`. Re-running these commands at the submitted head therefore shifts several
rows by one or two, and the totals move with them. The commands and the findings hold; the exact
integers belong to the commit named above.

One internal consistency check falls out of the excluded column: `--exclude-tests` drops exactly 357
files, and 266 TEST + 91 TEST_FIXTURE is exactly 357, leaving exactly the 277 PRODUCTION files. That
is agreement by construction rather than independent confirmation — the role classifier and the
exclusion predicate share the same path rules — but it does show the two are not drifting apart.

Two findings, both of which the graph produced and neither of which grep would have:

**`net/http` is a test-only dependency here.** All three files that import it are
`internal/sem/provider_parallel_matrix_test.go`, `internal/sem/provider_test.go`, and a fixture under
`internal/sem/testdata/`. The 78 At-Risk files were almost entirely other `_test.go` files calling
helpers in `provider_test.go`. With `--exclude-tests` the answer is a clean zero: a constraint
forbidding `net/http` would not touch a single shipped line of this provider. That empty result is the
report doing its job, not failing — and it is only legible because the excluded count is printed
alongside it.

**`os/exec` is the real one.** It is the no-egress boundary's neighbour, and the 8 production files
that reach it are named, not summarised:

```
cmd/graph-bench/main.go                             internal/gitutil/process_descendants_notwindows.go
internal/bench/worker.go                            internal/gitutil/process_job_notwindows.go
internal/cli/verify.go                              internal/gitutil/worktree_paths.go
internal/gitutil/git.go                             internal/sem/search_verify.go
```

Six of the eight are the git-subprocess and verification layers (`internal/gitutil/*`,
`internal/cli/verify.go`, `internal/sem/search_verify.go`); the remaining two are benchmark tooling
(`cmd/graph-bench`, `internal/bench`), which is a different kind of finding — a constraint author
would treat those very differently from code on the request path.

At `--depth 1` those 8 plus their 11 direct dependents are a 19-file shortlist a person can read in one
sitting — down from 319 files (32 invalidated + 287 at-risk) without the flags, a 94% reduction with
nothing that ships lost.

Verification is in `internal/cli/pivot_test.go`. The load-bearing case is `fixtures.go` in the test
fixture: production code whose only path to invalidated code runs through a test file. It is AT-RISK
without the flag and SAFE with it, which is what proves the exclusion happens before classification
rather than as a filter over the output.

## What the grouped report looks like

Roles turn the same classification into a ranked worklist. Run against this repository at commit
`53ff6bd`, `--dependency os/exec` with no other flags — 636 files, 32 invalidated, 287 at-risk:

```
Required remediation:  32 direct violation(s) (8 production, 24 test across 5 package(s))
Production follow-up:  100 at-risk production file(s)
Test fallout:          187 file(s) across 4 package(s)
Coverage gaps:         85 unreached
```

The four lines are the whole point. Nothing is hidden — the 32 violations are all still violations —
but the reader now knows that eight of them are files to open and 24 are five commands to run. The
187 at-risk test files, which the previous version printed in full, are four lines:

```
TEST FALLOUT (187 file(s) across 4 package(s)) — collapsed on purpose.
These are covered by running the package, not by reading the list:
- internal/sem                        126 file(s)   go test ./internal/sem
- internal/cli                         58 file(s)   go test ./internal/cli
- internal/bench                        2 file(s)   go test ./internal/bench
- internal/sem/testdata/fixtures/typescript-http    1 file(s)   review internal/sem/testdata/fixtures/typescript-http/ (no Go test target derivable)
```

The last line is the honest case: a package with no Go in it is told so rather than handed a command
that would fail.

## The work order: `--format plan`

The JSON report says what every file **is**. That is the right thing to archive and the wrong thing to
hand an agent, because an agent given a classification invents the plan itself — differently each
time, with no way afterwards to tell what it was supposed to have done. `--format plan` states the
work instead, as `pivot-plan/v1`.

Run at `--dependency os/exec --exclude-tests --depth 1` against `53ff6bd`: **278 work items — 8 P0,
11 P1, 259 P2**, with 7 required commands, 6 declared blind spots and 6 must-not-claim clauses.

The `plan_id` is deliberately not quoted here. It is a digest over the head commit, the constraint,
the depth, the flags and the counts, so it changes with every commit by design — that is what makes
two plans comparable or provably different. A fixed id printed in prose is stale the moment anything
lands, and a reader who cannot reproduce it has been given a number to distrust. Read it from the run
itself instead:

```sh
entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1 --format plan \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['plan_id'])"
```

**Every classified file becomes exactly one work item**, so nothing falls between the report and the
plan: `INVALIDATED → REPLACE_OR_REMOVE`, `AT-RISK → VERIFY_AFTER_ROOT_FIX`, `SAFE → NO_ACTION`,
`UNREACHED → MANUAL_REVIEW`. Safe files are included deliberately: a file that was analysed and
cleared is a different thing from a file nobody looked at, and only the plan can tell an agent which
one it is holding.

**Priority encodes what blocks what.** P0 is a direct violation in code that ships — nothing
downstream can be verified until it is gone. P1 is a violation in a test, or shipped code standing on
an invalidated foundation. P2 is covered by running a package, or is not a change at all. Items are
sorted before ids are assigned, so reading the plan top to bottom is reading it in execution order.

**The plan id is deterministic** — a digest of head commit, constraint, depth, flags and counts — so
the same question asked twice is recognisably one plan rather than two.

Two parts of the schema exist because of how an *agent* fails rather than how the graph works:

- **`known_blind_spots`** pairs each limitation with its consequence, and is stated *before* the work
  items, because an agent decides how far to trust a plan before it reads it, not after. Six entries:
  interface dispatch, reflection, generated code, inventory-only languages, name matching, and that
  SAFE means no path was found in this snapshot rather than proof of absence.
- **`execution_contract.must_not_claim`** names the likely failure directly. It is not a bad edit — a
  test catches that. It is a confident report of success nobody ran a command to check. The clauses
  forbid claiming the dependency is gone without re-running pivot, claiming tests pass without
  executing them, treating SAFE as proof of absence, and counting an UNREACHED file as resolved. The
  GENERATED and VENDOR clauses appear only when those roles are actually present, so the contract
  stays signal rather than boilerplate.

`validation.required_commands` is where the package collapse becomes executable: `go build ./...`,
one `go test ./pkg` per affected package, and last the exact pivot re-run that proves the invalidated
count reached zero.

One correctness fix came out of building this. Propagation now carries the originating invalidated
file along the walk. At two hops an at-risk item used to report the intermediate neighbour it was
reached through; `root_invalidated` now names the file that actually has to be fixed first, which is
what the plan orders work by — `internal/cli/callsite.go` points at `internal/gitutil/git.go`, not at
whatever it happened to be reached through.

## Noon Curveball: what changed and how we adapted

**The constraint.** *Graph is evidence, not an oracle.* A repository using dynamic dispatch,
generated code or reflection cannot be fully resolved by static analysis. The product must not
present incomplete relationships as certain, must identify when analysis may be partial, must offer
a verification path, must keep working unchanged for fully resolved code, and must let **users and
agents** tell apart confirmed structural evidence, heuristic or incomplete evidence, and claims
needing source or test verification.

### Every requirement, and where it is answered

| # | Requirement | Where |
|---|---|---|
| 1 | Must not present incomplete relationships as certain | every evidence line leads with `[CONFIRMED]` / `[HEURISTIC]` / `[UNVERIFIED]` (`pivot_roles.go`) |
| 2 | Must identify when analysis may be partial | `UNVERIFIED` verdict + `analysis_note` carrying the parser's own error code; evidence mix in the header |
| 3 | Must provide a safe fallback or verification path | per-file `VERIFY:` line for users, `verification_required: true` for agents |
| 4 | Existing behaviour for fully resolved code keeps working | `TestPivotFullyResolvedBehaviourUnchanged`; full suite green, no assertion modified; semantic diff purely additive |
| 5 | Test or fixture representing incomplete analysis | `pivotPartialAnalysisSnapshot` in `pivot_evidence_test.go`, one file per unresolvable pattern |
| 6 | Use Entire Graph to identify evidence consumers | `docs/demo/curveball/01-06`, all run **before** the first edit |
| 7 | Users **and agents** can tell the three apart | text tags + `VERIFY:` for users; `evidence_quality` + `verification_required` + 2 blind spots + 2 contract clauses for agents |

### Our own loop found this before the card named it

Reconstructed from `entire checkpoint explain 96c865a5662c`, not from memory. Full working and the
raw transcript excerpt in [`docs/demo/curveball/15-loop-findings-from-checkpoint.md`](docs/demo/curveball/15-loop-findings-from-checkpoint.md).

At 05:42Z the verification loop ran against gorilla/mux. The cold executor agent — no prior context,
handed only the work order — reported this back:

> WI-0003's evidence cites `source_file: regexp.go, line: 676` [...] but `regexp.go` is only 413
> lines [...] This specific evidence entry (**confidence 0.68, "method call matched globally unique
> method name" — a heuristic match**) does not hold up as cited.

A `name_only` edge at 0.68 produced a citation to a line that does not exist. The AT-RISK verdict
built on it was still correct, for unrelated reasons — `route.go:259` really does call
`newRouteRegexp` — which is what makes this the dangerous shape: **the wrong evidence and the right
answer arrived together.**

The agent caught it by opening the file and counting lines. Nothing in the output distinguished that
edge from an import declaration the parser had read directly. The graph had recorded
`resolution: name_only` and `confidence: 0.68` at the moment it created the edge. PivotMap printed
the number and acted on neither.

We fixed the symptom that morning (`0fa4a48`, evidence cited against the wrong file) and the class
after noon (`d09e6af`). The curveball did not introduce this problem to us; it named a problem our
own loop had already surfaced, and pushed us from patching one citation to grading every edge.

The same checkpoint carries two loop findings that remain **unbuilt**, stated in
[Known limitations](#known-limitations-and-next-steps) rather than quietly dropped: the loop is
gameable by deletion (`invalidated == 0` is satisfied by deleting the code — checked by hand on mux
at +7/-8, enforced nowhere), and it returns a boolean where it needs
`PASS / FAIL / PARTIAL / CANNOT_VERIFY`.

### The tempting wrong answer

PivotMap already looked compliant. It ships `UNREACHED` for inventory-only languages, it prints
"SAFE means no path was found, not that none exists", and its plan declares six blind spots that
name interface dispatch, reflection and generated code explicitly. The easy response was "we already
did this."

That response is wrong, and the reason is worth stating precisely: **every one of those is a global
statement, and none of them ever reached the file it applied to.** A reader looking at
`pkg/foo.go — SAFE` had no way to connect it to a disclaimer six sections earlier. We had declared
our uncertainty; we had never localised it.

### The assumption that was invalidated

Uncertainty was tiered at the **language** level and nowhere else. It was never tiered at the
**edge** level. Verified in source before changing anything:

- `Confidence` was captured into every `pivotEdge` (`pivot.go:428`, `:476`) and read by exactly one
  line in the whole feature — a `Fprintf` in `pivot_roles.go`. Printed, never consulted.
- `sem.RelationRecord.Resolution` and `.WarningCodes` — the graph's own account of *how* it knows an
  edge — were never copied into `pivotEdge` at all.
- `Header.Warnings` and `Header.PartialFailures` were copied into the response (`:348`, `:544`) and
  then read by nothing.

So these two rendered and behaved identically:

```
IMPORTS os/exec       confidence 0.80  resolution import declaration read from source
CALLS   verify.go     confidence 0.80  resolution package-level guess
```

**The consequence was live in this repository.** Five files were reported SAFE — "no dependency path
to invalidated code, leave alone" — while carrying `E_PARSE_ERROR`:

```
internal/sem/grammars/{csharp,erlang,fsharp,haskell,perl}/tree_sitter/array.h
```

The parser had failed on them. SAFE presented a gap in the analysis as a finding about the code,
which is exactly what the card forbids.

### What changed

Every edge is now graded, keyed on **the graph's own resolution vocabulary** rather than a threshold
we invented. The rules were chosen from a measured distribution over all 57,791 relations in this
repository, recorded in `internal/cli/pivot_evidence.go`:

| Tier | Means | Example |
|---|---|---|
| `CONFIRMED` | import declarations, and calls resolved to a definition (`exact`, `import_resolved`) at or above a 0.80 floor | `IMPORTS internal/gitutil/git.go -> os/exec` |
| `HEURISTIC` | inferred — `name_only`, `package`, `type_inferred`, `pattern` | `CALLS preflight.go -> verify.go (resolution package)` |
| `UNVERIFIED` | the parser reported a warning or partial failure on the file | `array.h`, `E_PARSE_ERROR` |

**Confidence alone could not carry this.** 0.80 covers both `IMPORTS name_only` — an import statement
physically present in the source — and `CALLS package`, a guess about where a call landed. Resolution
is the axis that separates a fact from an inference. And no relation in this repository reaches
confidence 1.00; the highest observed is 0.95, so a design waiting for certainty would have graded
everything uncertain.

Surfaced to both audiences the card names:

```
users   [CONFIRMED] evidence: IMPORTS internal/cli/verify.go -> os/exec (confidence 0.80, resolution name_only)
        [HEURISTIC] evidence: CALLS internal/cli/preflight.go -> internal/cli/verify.go (confidence 0.80, resolution package)
            VERIFY: HEURISTIC evidence — confirm in source or by test before acting on this verdict.

agents  "evidence_quality": "HEURISTIC", "verification_required": true
        known_blind_spots += heuristic-edges, unverified-parse
        must_not_claim    += "Must not treat a HEURISTIC edge as an established fact..."
```

A file that would have been SAFE but did not parse cleanly is now `UNVERIFIED`. A file that did not
parse but *has* a real path to invalidated code stays `AT-RISK` — the verdict still says there is a
path, because there is one — and carries its uncertainty as evidence quality instead.

### Why the new result is safe

Nothing about a clean parse changed. Evidence quality is graded after the verdicts are final,
exactly as roles are, and never moves one. `TestPivotFullyResolvedBehaviourUnchanged` pins all six
verdicts of the original fixture and asserts the unverified bucket stays empty. The full
`internal/cli` suite passes at 56.071s with **no existing assertion modified**.

The direction of every change is towards less certainty, never more: no file moves from a weaker
verdict to a stronger one, and `CONFIRMED` requires both a structural resolution and a confidence
floor. The semantic diff (`docs/demo/curveball/07-semantic-diff.txt`) is entirely additive —
new fields and functions, nothing removed or renamed.

### The fixture for incomplete analysis

`internal/cli/pivot_evidence_test.go` carries a partial-analysis fixture with one file per pattern
static analysis loses: an import read from source, a call matched only by name, generated code
carrying a warning, and a file the parser failed on. Nine tests cover the tier rules directly, both
output surfaces, the weakest-edge rule, and the fully-resolved regression lock.

### Run against Kubernetes

The card describes a repository "using dynamic dispatch, generated code, reflection, or another
pattern that static analysis cannot fully resolve". Kubernetes at `b2ec8b6f` is that repository,
counted rather than asserted: **17,838 Go files, 4,259 carrying a `DO NOT EDIT` header, 1,592
importing `reflect`, 3,754 interface declarations.**

```
pivot --dependency os/exec --exclude-tests --depth 1     # 608s cold parse

46 invalidated, 78 at-risk, 12877 safe, 48 unverified, 794 unreached (of 13843 files)
Evidence: 52 CONFIRMED, 71 HEURISTIC, 49 UNVERIFIED — verify the non-confirmed before acting
```

**More of the evidence was inferred than proved** — 71 heuristic against 52 confirmed. Before this
change all 172 edges rendered identically, and any one of them could have carried a P0 work item
alone.

One of the heuristic edges is demonstrably false, which is why it is in the record:

```
[HEURISTIC] evidence: USES_TYPE pkg/controller/garbagecollector/graph.go:142
            -> cmd/prune-junit-xml/prunexml.go (confidence 0.75, resolution name_only)
```

`graph.go:142` declares `func (n *node) setOwners(owners []metav1.OwnerReference)`.
`prunexml.go:309` declares `type owners struct`. The parser matched a **parameter name** to an
unrelated **type** in a different binary; `graph.go` does not import that package at all. The graph
was honest when it created the edge — it recorded `name_only` and `0.75` — and pivot used to discard
both. Full working in `docs/demo/curveball/11-kubernetes-finding.md`.

The other half is the **48 files previously folded into the safe count**: shell scripts and YAML the
parser could not read, reported as "no dependency path to invalidated code, leave alone".

### Graph evidence, captured before the first edit

Every graph command was run **before** any code changed, and piped to a file rather than described:
`docs/demo/curveball/01-search-evidence-consumers.txt`, three `impact` runs on the verdict maker,
the propagation walker and the renderer, the `neighbors` query that revealed per-edge `resolution`
and `confidence`, `capabilities`, and pivot run against itself.

Three findings are recorded there rather than asserted from memory:

1. The `Completeness: degraded for Go` note from before noon **no longer reproduces** — this
   repository reports `completeness_level: "ok"` for Go with one JSON partial failure. The earlier
   note was carried forward and is stale; it is not repeated.
2. `pivot --dependency internal/sem` returns **0 invalidated**: `prohibitedMatch` does not resolve
   internal package paths. Recorded as a finding about the matcher rather than reported as an empty
   result.
3. The installed plugin has no `pivot` subcommand — the feature exists only in this working tree —
   so all pivot runs use a locally built binary without the `graph` prefix. Built-in commands
   (`search`, `impact`, `neighbors`, `diff`, `capabilities`) run fine on the installed v0.4.0.

Finding 2 was fixed later in the same session, once the graded work was committed and pushed: see
`fix(pivot): resolve internal package paths, so zero means zero`. The same run now returns 139
invalidated files, which is the answer to the card's question about which parts of this
implementation consume relationship evidence.

### The evidence folder

Everything above is reproducible from `docs/demo/curveball/`, committed rather than described:

| File | What it is |
|---|---|
| `PLAN.md` | the plan for this session, its steps ticked off, and the findings recorded as they were made |
| `01-search-evidence-consumers.txt` | `search` — located the code consuming relationship evidence |
| `02..04-impact-*.txt` | `impact` on the verdict maker, the propagation walker, the renderer |
| `05-neighbors-firstEvidenceLine.txt` | `neighbors` — the run that revealed per-edge `resolution` and `confidence` |
| `06-capabilities.json` | semantic vs inventory-only languages |
| `07/08-semantic-diff.*` | `diff 90d1939..HEAD` — the adaptation, entity by entity, entirely additive |
| `10-kubernetes-pivot.txt` | the full Kubernetes run |
| `11-kubernetes-finding.md` | the false edge, checked against source, with the limits of the run stated |
| `12-impact-prohibitedMatch.txt` | `impact` captured before the matcher fix |
| `13/14-pivot-on-itself-*` | PivotMap answering the card's question about itself, after the fix |

Items 01-06 all ran **before the first line of code changed**.

## Checkpoint links and what each checkpoint proves

Every commit below is on `pivot-work-order`. `main` is protected on the Entire mirror, so the branch
lands via PR; the checkpoint ref (`entire/checkpoints/v1`) pushes independently of it.

| Checkpoint | Commit | Milestone | What it proves |
|---|---|---|---|
| — | `abbf061` | — | Classifier, propagation, evidence output. **No checkpoint** — see below. |
| — | `4a0a219` | — | `--exclude-tests` applied before classification. **No checkpoint** — see below. |
| `bb0d72d79dbc` | `2ff8446` | Initial understanding and architecture | Checkpoints bind from this worktree. The header keeps depth and index provenance on uncommitted runs, so a reader can tell how far propagation walked and whether the verdict came off a warm cache. |
| `4a14d2327199` | `934d180` | Last stable state before noon | File roles rank the report instead of listing it: 32 direct violations resolve to 8 production files to open by hand and 24 test files collapsed to 5 package commands. |
| `9ab0f807a913` | `90d1939` | Last stable before the Noon Curveball | The commit message carries intent, architecture, completed work and open risks, so a fresh session reconstructs the project from the checkpoint rather than from a summary pasted out of the previous one. That is how this curveball response started. |
| `c8f225bf19ba` | `d09e6af` | Curveball response: evidence tiers | Uncertainty moves from the language level to the edge level. Confidence was captured and never consulted; `Resolution` was dropped entirely; parse failures were copied into the response and read by nothing. Five files in this repository were reported SAFE while carrying `E_PARSE_ERROR`. |

### The two commits with no checkpoint are the honest part

`abbf061` and `4a0a219` carry no checkpoint, and that is worth stating rather than hiding. Entire's
hooks live in `.claude/settings.json` and load only when the agent session is launched from inside
that directory. Two earlier sessions were launched from the parent folder, never loaded the hooks,
and produced commits with no checkpoint attached — while `entire doctor` reported green throughout,
because it inspects the repository's settings file and cannot see where the agent was started from.

The failure is silent by construction, which is exactly the class of problem this project is about: a
tool reporting healthy is not the same as a tool having checked the thing you care about. The fix was
to verify the binding directly (`entire session current` naming a session, then `entire checkpoint
list` showing a non-zero count against a real commit) rather than trusting the diagnostic. Every
commit from `2ff8446` onward carries a checkpoint, verified that way.

## Setup, run and test instructions

```sh
mise run build          # builds ./entire-graph (needs Go + CGO for tree-sitter)
mise run test           # go test ./...

entire graph pivot --repo . --dependency net/http
entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1
entire graph pivot --repo . --dependency net/http --checkpoint <id> --format json

# the agent work order
entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1 --format plan
```

`mise run test` does not come back fully green on every machine: `cmd/graph-bench`'s clone helper
tests fail wherever the local git rejects `checkout --end-of-options`. Those failures reproduce at
`4a0a219`, before any of this work, and are unrelated to pivot — `internal/cli`, which is the only
package this feature touches, passes in full.

## Known limitations and next steps

- **Static analysis cannot see dynamic dispatch.** A call through an interface, reflection, or
  generated code leaves no edge to follow, so a genuinely affected file can be reported Safe. This is a
  property of the underlying parser, not a bug in pivot, and it is the reason the report ends by saying
  Safe means "no path found," not "no path exists." Observed concretely: `impact --symbol Route.Match`
  on `gorilla/mux` reports 0 callers even though `Router.Match` calls it through an interface.
- **Constraint interpretation is out of scope by design.** Pivot takes a package name, not a sentence.
  The natural-language step belongs outside the no-egress boundary.
- **Prohibited-name matching is path-shaped** (exact, a `pkg/` prefix, or a package path segment
  inside a symbol id). A dependency referred to by an alias in source is still not resolved to its
  canonical package. Internal package paths were also unmatched until the curveball session: a
  constraint naming `internal/sem` returned zero invalidated files on a repository where 139 files
  depend on it, and zero reads as compliance. Fixed, with the id shapes the graph actually emits
  pinned in `TestProhibitedMatchResolvesInternalPackagePaths`.
- **Test exclusion is path-shaped, not build-tag-shaped.** `--exclude-tests` recognizes conventional
  test paths (`_test.go`, `*.test.*`, a `test/`/`testdata/` directory segment, and the equivalents in
  the other supported languages). A test helper that lives in a normally-named file is still
  classified, and a production file that happens to sit under `testdata/` is still excluded.
- **The verification loop is gameable by deletion, and nothing enforces otherwise.** `invalidated == 0`
  is satisfied by deleting the code as well as by fixing it — the failure mode the loop research names
  as "remove code rather than repair it". It was checked by hand on the mux run (`1 file, +7/-8, no
  test touched, suite green before and after`) and that check lives in a human's head, not in code.
  The defence is a deletion budget plus retained-behaviour assertions, roughly 30 lines.
- **The loop reports a boolean where it needs four statuses.** `PASS / FAIL / PARTIAL /
  CANNOT_VERIFY`. A missing tool or a timeout is not a pass, in the same way a file that did not parse
  is not a file that came back Safe — `CANNOT_VERIFY` is this project's own `UNREACHED`, applied to
  the verifier instead of the classifier. Both findings are recorded in
  `docs/demo/curveball/15-loop-findings-from-checkpoint.md` with their checkpoint source.
- **Next step:** severity beyond the three buckets — an At-Risk file whose only link is a type
  reference is a much smaller job than one that calls an invalidated function on every request path,
  and the graph already carries enough to tell those apart.
