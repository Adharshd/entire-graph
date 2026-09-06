# What the verification loop found, hours before the curveball named it

Source: `entire checkpoint explain 96c865a5662c` (session `fea6b82a`, commit `5a7a3dd`,
2026-09-06 05:42Z). Reconstructed from the checkpoint in the post-curveball session
rather than remembered — this is the transcript, not a summary of it.

## Finding 1 — a heuristic edge produced a citation to a line that does not exist

The cold executor agent, running the mux work order with no prior context, reported:

> WI-0003's evidence cites `source_file: regexp.go, line: 676` for a `CALLS` relation
> into route.go, but `regexp.go` is only 413 lines (415 before my edit) — line 676
> doesn't exist in that file. [...] This specific evidence entry (**confidence 0.68,
> "method call matched globally unique method name" — a heuristic match**) does not hold
> up as cited. That said, route.go's *actual* dependency on regexp.go is real and
> separately verifiable (`route.go:68` type-asserts `*routeRegexp`, `route.go:259` calls
> `newRouteRegexp(...)`), so the AT-RISK classification itself is still correct — only
> the specific cited evidence line is wrong.

**This is the Noon Curveball, found five hours before the card described it.** A
`name_only` edge at confidence 0.68 produced a false citation. The verdict built on it
happened to be right for other reasons, which is exactly what makes the failure mode
dangerous: the wrong evidence and the right answer arrived together.

The agent caught it by opening the file and counting lines. Nothing in the output
distinguished that edge from an import declaration the parser had read directly — the
graph knew (`resolution: name_only`, `confidence: 0.68`) and pivot printed the number
without acting on it.

Two fixes came out of this, five hours apart:

- `0fa4a48` fixed the **symptom** — evidence was being cited against the wrong file.
- `d09e6af` fixed the **class** — that edge now prints
  `[HEURISTIC] ... resolution name_only` with a `VERIFY:` line under the file, and the
  work item carries `evidence_quality: "HEURISTIC"` and `verification_required: true`.

## Finding 2 — the loop can be gamed by deletion

> our loop can be gamed. `invalidated == 0` is satisfied by *deleting the code* just as
> well as by fixing it. The research names that exact attack — "remove code rather than
> repair it" — and the defence is a deletion budget plus retained-behaviour checks. Our
> executor agent was told not to do that in the brief, but nothing *enforces* it.

Checked by hand on the mux run, and it held:

```
regexp.go | 15 +++++++--------      1 file changed, 7 insertions(+), 8 deletions(-)
```

The count alone was never the evidence. One file, +7/-8, no test touched, suite green
before and after. **Still unenforced in code** — see next steps.

## Finding 3 — the loop returns a boolean where it needs four statuses

> replace the hard-coded `invalidated == 0` with a declarative contract of named checks;
> run it from a clean checkout the agent can't write to; report **four** statuses, not a
> boolean — `PASS / FAIL / PARTIAL / CANNOT_VERIFY`.

> a missing tool or a timeout is not a pass. Cheap, and it's the same epistemic move
> PivotMap already makes with `UNREACHED` vs `SAFE`.

`CANNOT_VERIFY` is the curveball applied to the verifier instead of the classifier. A
check that could not run is not a check that passed, in the same way that a file that
did not parse is not a file that came back Safe. **Not built.**

## Raw transcript excerpt

The lines above, unedited, as `checkpoint explain` returned them:

```
[Assistant] **The research in three lines:** replace the hard-coded `invalidated == 0` with a declarative contract of named checks (`command`, `graph_count`, `metric_delta`, `path_integrity`); run it from a clean checkout the agent can't write to; report **four** statuses, not a boolean — `PASS / FAIL / PARTIAL / CANNOT_VERIFY`.

**The part that actually bites us:** our loop can be gamed. `invalidated == 0` is satisfied by *deleting the code* just as well as by fixing it. The research names that exact attack — "remove code rather than repair it" — and the defence is a deletion budget plus retained-behaviour checks. Our executor agent was told not to do that in the brief, but nothing *enforces* it.

**My call: don't build the config system now.** It's a real feature, not a 30-minute one, and the gate is at 11:45. Two things are worth stealing cheaply though:

1. **`CANNOT_VERIFY`** as a distinct status — a missing tool or a timeout is not a pass. Cheap, and it's the same epistemic move PivotMap already makes with `UNREACHED` vs `SAFE`.
2. **The anti-gaming check** — when the loop re-runs, also assert the agent didn't just delete the file.

  - `test_helpers.go` calls `requestWithVars` (mux.go) — untouched, no diff needed.
- Outcome for all four: `no_change_needed` — the root fix kept the same interface, exactly the scenario the plan calls out as the correct "no diff" outcome.
- **Evidence discrepancy to flag**: WI-0003's evidence cites `source_file: regexp.go, line: 676` for a `CALLS` relation into route.go, but `regexp.go` is only 413 lines (415 before my edit) — line 676 doesn't exist in that file. Line 676 of `mux.go` is `methodNotAllowedHandler`, unrelated to regexp or route.go. This specific evidence entry (confidence 0.68, "method call matched globally unique method name" — a heuristic match) does not hold up as cited. That said, route.go's *actual* dependency on regexp.go is real and separately verifiable (`route.go:68` type-asserts `*routeRegexp`, `route.go:259` calls `newRouteRegexp(...)`), so the AT-RISK classification itself is still correct — only the specific cited evidence line is wrong. This doesn't change the outcome (no edit needed either way) but is worth a human's attention if the evidence field is meant to be load-bearing elsewhere.

**Items not touched, and why:**
[Assistant] **The loop closed.** But the research warned `invalidated == 0` is satisfiable by deletion — so let me check the agent didn't just delete the code.

[Tool] Bash: Verify agent did not game the check by deleting code

[Assistant] **The loop works, end to end.**

```
metric         BEFORE    AFTER
invalidated         1        0   <-- 
at_risk             4        0   <-- 
safe                5       10
p0                  1        0   <-- 
p1                  4        0   <-- 

before head: db9d1d0073d2   plan: pivot-1a00c194311f
after  head: 31bca9f01579   plan: pivot-57f44b44af34
```

**And it wasn't gamed.** The anti-deletion check the research warned about:

```
regexp.go | 15 +++++++--------      1 file changed, 7 insertions(+), 8 deletions(-)
M  regexp.go                        <- one file, no tests touched
go test ./...  -> ok                <- green before AND after
```

Smallest complete response. Tests. Commit. Verify the trailer.

### 13:30–14:15 · The two hardening wins from the research
1. **`CANNOT_VERIFY` as a fourth status** — a missing tool or timeout is not a pass. Cheap, and it mirrors the `UNREACHED` vs `SAFE` distinction the tool already makes. Strongest single addition.
2. **Anti-gaming deletion check** — assert the fix didn't just delete the code. We ran this by hand today (`+7/-8, no tests touched`); making it a real check is maybe 30 lines.
3. *If time:* fix the evidence line-number bug.

### 14:15–14:40 · Docs + checkpoint 4
```
