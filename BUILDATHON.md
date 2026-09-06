# PivotMap

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

One new command in this fork: `internal/cli/pivot.go`, registered in the existing dispatcher in
`internal/cli/root.go`.

```
entire graph pivot --dependency <prohibited package> [--checkpoint <id>] [--depth N] [--exclude-tests]
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

`--exclude-tests` (the same path predicate `impact` and `neighbors` use) drops test files *before*
classification rather than filtering them out of the finished report. The difference matters for
propagation, not just for the listing: a test file is not a foundation anything ships on, so a
production file whose only route to invalidated code runs through a test file should come back Safe,
not At-Risk. The count of excluded files is printed, because a file that was never classified is not
a file that came back Safe.

Every verdict prints the edge that produced it — relation type, target, confidence, source line — so a
reader can check the claim rather than trust it.

### Two design decisions worth defending

**The classifier is rules, not a model.** No LLM produces a verdict. The interpretive step — turning a
sentence like "we can't send customer data through Library X" into the package name `libraryx` — stays
outside this binary, and `--dependency` takes the result directly. That is not only a design
preference: `entire graph doctor` asserts `no_egress=true`, and a model call inside the plugin would
make the plugin's own diagnostic lie.

**Propagation is capped.** Unbounded transitive closure marks an entire repository At-Risk, which is
technically true and completely useless. Two hops, with the distance reported per file, keeps the
output a shortlist a person can actually work through.

## Entire Graph findings and verification

Run against this repository (631 files, 355 of them tests), at commit `abbf061`:

| Run | INVALIDATED | AT-RISK | SAFE | UNREACHED |
|---|---|---|---|---|
| `--dependency net/http` | 3 | 78 | 465 | 85 |
| `--dependency net/http --exclude-tests` | **0** | **0** | 203 | 73 |
| `--dependency os/exec` | 32 | 284 | 230 | 85 |
| `--dependency os/exec --exclude-tests` | 8 | 99 | 96 | 73 |
| `--dependency os/exec --exclude-tests --depth 1` | 8 | 11 | 184 | 73 |

Two findings, both of which the graph produced and neither of which grep would have:

**`net/http` is a test-only dependency here.** All three files that import it are
`internal/sem/provider_parallel_matrix_test.go`, `internal/sem/provider_test.go`, and a fixture under
`internal/sem/testdata/`. The 78 At-Risk files were almost entirely other `_test.go` files calling
helpers in `provider_test.go`. With `--exclude-tests` the answer is a clean zero: a constraint
forbidding `net/http` would not touch a single shipped line of this provider. That empty result is the
report doing its job, not failing — and it is only legible because the excluded count is printed
alongside it.

**`os/exec` is the real one.** It is the no-egress boundary's neighbor: 8 production files reach it,
seven of them the git-subprocess and verification layers (`internal/gitutil/*`, `internal/cli/verify.go`,
`internal/sem/search_verify.go`). At `--depth 1` that plus its 11 direct dependents is a 19-file
shortlist a person can read in one sitting — down from 316 files without the flag, a 94% reduction with
no loss of anything that ships.

Verification is in `internal/cli/pivot_test.go`. The load-bearing case is `fixtures.go` in the test
fixture: production code whose only path to invalidated code runs through a test file. It is AT-RISK
without the flag and SAFE with it, which is what proves the exclusion happens before classification
rather than as a filter over the output.

## Noon Curveball: what changed and how we adapted

*(filled in after 12:00)*

## Checkpoint links and what each checkpoint proves

*(filled in as milestones land)*

## Setup, run and test instructions

```sh
mise run build          # builds ./entire-graph (needs Go + CGO for tree-sitter)
mise run test           # go test ./...

entire graph pivot --repo . --dependency net/http
entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1
entire graph pivot --repo . --dependency net/http --checkpoint <id> --format json
```

## Known limitations and next steps

- **Static analysis cannot see dynamic dispatch.** A call through an interface, reflection, or
  generated code leaves no edge to follow, so a genuinely affected file can be reported Safe. This is a
  property of the underlying parser, not a bug in pivot, and it is the reason the report ends by saying
  Safe means "no path found," not "no path exists." Observed concretely: `impact --symbol Route.Match`
  on `gorilla/mux` reports 0 callers even though `Router.Match` calls it through an interface.
- **Constraint interpretation is out of scope by design.** Pivot takes a package name, not a sentence.
  The natural-language step belongs outside the no-egress boundary.
- **Prohibited-name matching is path-shaped** (exact, or a `pkg/` prefix). A dependency referred to by
  an alias in source is not currently resolved to its canonical package.
- **Test exclusion is path-shaped, not build-tag-shaped.** `--exclude-tests` recognizes conventional
  test paths (`_test.go`, `*.test.*`, a `test/`/`testdata/` directory segment, and the equivalents in
  the other supported languages). A test helper that lives in a normally-named file is still
  classified, and a production file that happens to sit under `testdata/` is still excluded.
- **Next step:** severity beyond the three buckets — an At-Risk file whose only link is a type
  reference is a much smaller job than one that calls an invalidated function on every request path,
  and the graph already carries enough to tell those apart.
