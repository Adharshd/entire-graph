---
name: what-survives
description: >
  Answer what still stands when a requirement changes after the code was written.
  Use when a constraint arrives from outside the codebase — "we can't use X anymore",
  "legal dropped this vendor", "this library may no longer touch customer data",
  "compliance banned this dependency", "we're removing Y" — and someone needs to know
  which code breaks the new rule, what is built on top of it, and what can be left
  alone. Read-only, local, no network. The companion to what-happened: that one asks
  why code looks the way it does, this one asks what survives a rule that changed
  after it was written.
---

# what survives

A constraint landed after the work was done. Three questions follow, in this order:
which code actually violates it, what else breaks if that is ripped out, and what can
be left alone. `entire graph pivot` answers all three from the code graph and prints
the edge behind every verdict, so each claim can be checked rather than trusted.

## Build it first

`pivot` is not in the released plugin. Running `entire graph pivot` will fail with
`unknown command "pivot"`. Build the binary from this repository and call it directly —
note there is **no `graph` prefix** on the standalone binary:

```sh
go build -o ./entire-graph ./cmd/entire-graph    # needs CGO for tree-sitter
./entire-graph pivot --repo . --dependency os/exec --exclude-tests --depth 1
```

If the build fails, say so and stop. Do not fall back to grep and present the result as
if it were this analysis — the whole value here is the evidence attached to each verdict,
and grep has none.

## Run it

```sh
./entire-graph pivot --repo . --dependency <package>                       # everything
./entire-graph pivot --repo . --dependency <package> --exclude-tests --depth 1
./entire-graph pivot --repo . --dependency <package> --format plan         # for an agent
./entire-graph pivot --repo . --dependency <package> --format json         # everything, archived
```

A constraint has two halves — **"X may no longer reach Y"**. `--dependency` is Y, `--from` is X:

```sh
./entire-graph pivot --repo . --dependency <package>                  # nothing anywhere may reach it
./entire-graph pivot --repo . --from <path> --dependency <package>    # only this layer is governed
```

Use `--from` whenever the constraint names a layer, module, or directory — "the API layer may not
touch storage" is a rule about the API layer, and running it repo-wide returns every other
dependency on storage as if it were a violation. Omit it for a vendor removal, where the rule really
does apply everywhere.

`--dependency` takes a **package name, not a sentence**. Turning "we can't send customer
data through Library X" into `libraryx` is your job, and if the phrasing maps to more than
one package in this repo, ask which one rather than picking. An impact claim built on a
guessed target is worse than no answer.

`--exclude-tests --depth 1` is the right default for a first pass on a large repository:
it drops test files before classification rather than filtering them out afterwards, and
one hop keeps the result a shortlist a person can read in one sitting. Widen only when the
shortlist is genuinely too small to explain the problem.

## Read the verdicts

| Verdict | Means | What to do |
|---|---|---|
| `INVALIDATED` | this file reaches the prohibited dependency itself | must change; work these first |
| `AT-RISK` | it depends on something invalidated | recheck after the roots are fixed, not before |
| `SAFE` | parsed cleanly, no path found | leave alone |
| `UNVERIFIED` | the parser reported an error or warning on this file | **not cleared** — it was never successfully read |
| `UNREACHED` | inventory-only language, no relations parsed | unknown, not safe |

`SAFE` means no path was found in this snapshot. It is not proof that none exists. Calls
through interfaces, reflection, and generated code leave no edge to follow, so a genuinely
affected file can come back Safe. Say that out loud when reporting; do not quietly upgrade
it to "nothing else is affected".

## Read the evidence tier before acting on anything

Every evidence line is graded, because the graph knows the difference between a fact it
read and a name it matched — and before this existed, both printed identically.

```
[CONFIRMED]  evidence: IMPORTS internal/cli/verify.go -> os/exec (confidence 0.80, resolution name_only)
[HEURISTIC]  evidence: CALLS internal/cli/preflight.go -> internal/cli/verify.go (confidence 0.80, resolution package)
    VERIFY: HEURISTIC evidence — confirm in source or by test before acting on this verdict.
```

- **`CONFIRMED`** — an import declaration read from source, or a call resolved to a
  definition. Act on it.
- **`HEURISTIC`** — inferred from a name, a package, or a guessed receiver type. **Open the
  cited line and confirm the relation before you edit anything.** These are often right and
  are never proof. A real example from kubernetes: a `USES_TYPE` edge matched a *parameter*
  named `owners` to an unrelated *type* named `owners` in a different binary, in a file that
  does not import that package at all.
- **`UNVERIFIED`** — the file did not parse. The edge may be fine; what is unknown is the
  edges that are missing beside it.

Do not compare the confidence numbers. They do not separate a fact from a guess: an import
statement physically present in the source and a package-level guess both sit at 0.80.
Resolution is the axis that separates them, which is what the tier is derived from.

When a file carries a `VERIFY:` line, the verdict is not actionable on its own. Read the
source at the cited location first.

## Working the plan as an agent

`--format plan` emits `pivot-plan/v1`: one work item per classified file, ordered so that
reading it top to bottom is reading it in execution order.

Read these **before** the work items, not after — the decision about how far to trust a
plan is made before it is read:

- `known_blind_spots` — what the graph cannot see, each paired with its consequence.
- `execution_contract.must_not_claim` — what you may not assert when you are done.
- `execution_contract.must_not_do` — how this plan can be satisfied without being done.
- `validation.invariants` — the same rules as machine-checkable assertions.

Then: work `P0` before `P1`. An at-risk file cannot be verified until the root it depends on
is replaced, so fixing it first produces churn rather than progress.

Each item carries `evidence_quality` and `verification_required`. **Anything not `CONFIRMED`
requires the cited source line read, or a test run, before the item is acted on or reported
done.**

## Prove it, and do not stop at the count

Re-run the same command with the same flags and show the invalidated count reaching zero.
Then show the diff beside it:

```sh
./entire-graph pivot --repo . --dependency <package> --exclude-tests --depth 1   # count
git diff --stat                                                                  # the other half
```

**The count alone is not evidence.** `invalidated == 0` is satisfied by deleting the code
just as well as by replacing it. A healthy result looks like lines replaced rather than
removed, with no test file touched to get there — on the gorilla/mux run that was
`1 file changed, 7 insertions(+), 8 deletions(-)`, suite green before and after.

`scripts/pivot-verify.py` checks this mechanically against the plan's invariants and returns
`PASS` / `FAIL` / `PARTIAL` / `CANNOT_VERIFY`:

```sh
python3 scripts/pivot-verify.py --before before.json --repo . --rev-range HEAD~1..HEAD
```

`PARTIAL` is the normal good outcome, not a failure — whether behaviour was preserved is not
decidable from a diff. `CANNOT_VERIFY` means a check could not run; report which one and why.
It is never a pass.

## When not to use this

- The user already named the file and it is small. Read it. The graph earns its cost by
  removing exploration, and there is nothing to explore.
- The question is "why is this code like this" rather than "does it still stand". That is
  `what-happened` and `explain`, which read the checkpoint transcripts instead.
- The dependency is referred to by an alias in source rather than its canonical package
  name. Matching is path-shaped and will not resolve the alias; say so rather than
  reporting the resulting empty result as a clean bill of health.

## Never claim

- That the dependency is gone, without re-running pivot and showing the count.
- That tests pass, without having executed them and read the output.
- That `SAFE` proves no dependency path exists. It means none was found in this snapshot.
- That an `UNREACHED` or `UNVERIFIED` file is resolved. Neither was analysed.
- That a `HEURISTIC` edge is an established fact.
