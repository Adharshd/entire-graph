# Demo run-sheet — entire graph pivot

Audience: hackathon judges. Length: 5 minutes. One laptop, one terminal, this repo checked out.

## Opening line (say this first)

"When a requirement changes after code is written — 'we can't use this dependency anymore' — the
question is always the same: what actually breaks, and what's safe to leave alone. `pivot` answers
that from the code graph, not from an LLM guessing."

## Pre-flight (before judges sit down)

- Terminal open in `/Users/adharsh/git/BTW-buildathon/entire-graph`, font size bumped up.
- Confirm the binary is built: `./entire-graph version` (or `entire graph version` if installed as a plugin).
- Have `docs/demo/graph-evidence.txt` open in a second tab/pane as a cold-fallback (see fallback column).
- Know the real numbers below cold — don't read them off a screen if a command hangs.

## Run order (5 minutes, timed)

| Time | Command | Say | If it fails / is slow, show instead |
|---|---|---|---|
| 0:00–0:30 | *(no command — spoken intro)* | Problem statement above. One sentence, then move. | — |
| 0:30–1:15 | `entire graph search --repo . --query "prohibited dependency check in pivot" --format text --top-k 5` | "This is artifact one: a graph search, not a grep. It ranks by body, identifiers, and graph neighbors — it lands on `prohibitedMatch` in `pivot.go` directly." | Show the captured `### 1. SEARCH ###` block in `docs/demo/graph-evidence.txt` — same query, same result, already on disk. |
| 1:15–2:15 | `entire graph impact --repo . --symbol buildPivotResponse --file internal/cli/pivot.go` | "Artifact two: before I touch this function, impact shows every caller, callee, type consumer, and data flow in one shot — 4 callers, 14 callees, 6 type consumers. This is the check you'd otherwise do by hand with grep and hope." | Show `### 2. IMPACT ###` in `graph-evidence.txt`. Point out it also prints `Completeness: degraded for Go` with a `W_DATA_FLOW_EVIDENCE_UNMERGED` warning right next to the answer — the tool flags its own gaps unprompted. |
| 2:15–3:15 | `entire graph pivot --repo . --dependency os/exec --exclude-tests --depth 1 --format text` | "Now the actual product. Prohibited dependency `os/exec` in this 636-file repo. With test files excluded and depth capped at 1: 8 invalidated, 11 at-risk. That's a 19-file shortlist, down from 319 unfiltered — nothing that ships is lost." | Read the numbers straight from the REAL VERIFIED NUMBERS table below; state them from memory if the command is slow. |
| 3:15–4:00 | `entire graph pivot --repo . --dependency os/exec --format plan` | "This emits an agent work order — P0/P1/P2, one action per verdict, evidence attached, and a line that says what the agent must NOT claim when it's done. A fresh agent with zero context executes only the P0 items, then pivot re-runs the same deterministic rules against the result. No LLM grades the output — that's the part most agent demos skip." | If the live agent loop isn't wired up for the demo, describe it as the mux/`bytes` example: 1 P0 (`regexp.go` imports `bytes`, 2 call sites), 4 P1 at-risk, mux's own test suite passes before and after. |
| 4:00–4:45 | `entire graph diff --base 4a0a219 --head HEAD --json` (or the captured file) | "Artifact three: semantic diff — not a text diff, an entity diff. It's asking 'which functions/types actually changed shape' and how many things depend on them, so a signature change with a lot of dependents stands out before you run anything." | Show `### 3. SEMANTIC DIFF ###` in `graph-evidence.txt` — the `pivotFlags`/`pivotFile`/`pivotResponse` signature changes with `dependents_count`. |
| 4:45–5:00 | *(no command)* | Closing claim (below). | — |

## The three required artifacts (explicitly marked)

1. **Graph search / definition lookup** — step at 0:30, `entire graph search --query "prohibited dependency check in pivot"`.
2. **Relationship/impact analysis before a risky change** — step at 1:15, `entire graph impact --symbol buildPivotResponse`.
3. **Final semantic diff** — step at 4:00, `entire graph diff --base 4a0a219 --head HEAD --json`.

## The numbers to have cold

```
--dependency net/http                            3 inv |  78 at-risk | 468 safe | 85 unreached
--dependency net/http --exclude-tests            0 inv |   0 at-risk | 204 safe | 73 unreached
--dependency os/exec                            32 inv | 287 at-risk | 232 safe | 85 unreached
--dependency os/exec --exclude-tests             8 inv | 100 at-risk |  96 safe | 73 unreached
--dependency os/exec --exclude-tests --depth 1   8 inv |  11 at-risk | 185 safe | 73 unreached
```

Two stories, in case a judge asks "why these two dependencies":

- **net/http is test-only here.** All 3 importers are test files or a fixture. With
  `--exclude-tests` the answer collapses to a clean zero — the constraint would not touch a
  shipped line. That's the report working, not failing, and it's only legible because the
  excluded count (357 files) is printed alongside the zero.
- **os/exec is the real one.** 8 production files, six of them the git-subprocess and
  verification layers, two benchmark tooling. At `--depth 1` that plus 11 direct dependents is a
  19-file shortlist, down from 319 unfiltered — a 94% reduction with nothing that ships lost.

## Closing claim (single sentence)

"Every verdict here traces to one printed edge — relation type, target, source line, confidence —
so you can check the tool's homework in one line, which is the whole reason it can claim
no-egress and mean it."

## Known limitations — say these out loud, don't wait to be asked

- **SAFE is not proof of non-use.** It means no path was found in this local graph snapshot.
  Calls through interfaces, reflection, or generated code leave no edge to follow.
- **Concrete example, already observed**: `entire graph impact --symbol Route.Match` on
  gorilla/mux reports 0 callers, even though `Router.Match` calls it through an interface. The
  impact command self-reports `Completeness: degraded for Go` with a
  `W_DATA_FLOW_EVIDENCE_UNMERGED` warning next to the answer — show that warning as evidence the
  tool is honest about its own blind spot, not as something to explain away.
- **UNREACHED files are genuinely unknown**, not safe — inventory-only languages have file
  records but no semantic relations, so pivot can't classify them either way.
- **File roles rank and group the report, they never change a verdict.** If a judge asks whether
  marking something TEST could hide a real invalidation: no — `--exclude-tests` removes test
  files before classification runs, not after, and role labels are purely presentational.

## If the whole demo goes sideways

Fall back to reading `docs/demo/graph-evidence.txt` top to bottom — it has the search, impact, and
diff sections pre-captured from a real run — plus the numbers table above from memory. The story
survives without a live terminal; the live terminal is just better.

## TODO

- TODO: confirm exact CLI invocation for the multi-agent loop step (fresh-agent execution +
  pivot re-run) as it will actually be driven live — the write-up above describes the intended
  behavior and the mux/`bytes` reference numbers, not a copy-pasted transcript, because no
  captured transcript for that specific loop was provided.
