# Post-curveball plan

Written 12:00, 6 September. Submission closes **15:00 IST**. Three hours.

State going in: branch `pivot-work-order` at `cf70f83`, clean, pushed, in sync.
15 checkpoints across 3 sessions. `internal/cli` green (55s).

---

## Block 1 · Curveball response — mandatory, do not compress

Worth **15 points**. Everything else is upside.

**Start a fresh agent session.** Do not carry context over. Reconstructing from
checkpoints is the graded behaviour, and there are 15 checkpoints to reconstruct from.

```sh
entire checkpoint list                              # reconstruct, don't remember
entire graph search --repo . --profile full --query "<what the constraint touches>"
entire graph impact --repo . --symbol <X>           # BEFORE the first edit
# ... smallest complete change + tests ...
git commit                                          # verify Entire-Checkpoint trailer
git push origin pivot-work-order
```

**Step 3 is the one that gets skipped under pressure.** The guide explicitly asks for
impact analysis *before* the change, and it is one of the three required artifacts.
Capture the output to `docs/demo/` — a graph command run after the edit does not prove
the same thing.

Checklist:

- [ ] Fresh session started (not a continuation)
- [ ] Reconstructed from checkpoints, not from memory
- [ ] `impact` captured **before** the first edit
- [ ] Smallest complete change, not a redesign
- [ ] Tests run, real output read
- [ ] Committed, trailer verified, pushed
- [ ] Checkpoint 3 confirmed present

---

## Block 2 · One addition, not four

Ranked by score-per-minute. **Pick one.** If the curveball eats this block, ship zero
and say so under next steps — a stated plan beats a half-built feature.

| # | Addition | Why it earns its place | Cost |
|---|---|---|---|
| **1** | **`CANNOT_VERIFY` as a fourth status** | The tool already ships `UNREACHED` because "unknown is not safe". Extending that to the verifier is one coherent idea told twice, not a bolt-on. A timeout, a missing tool or a parser crash is not a pass. | ~30 min |
| 2 | **Enforced anti-deletion check** | Closes the real hole: `invalidated == 0` is equally satisfied by deleting the code. This was checked by hand during the mux loop (`1 file, +7/-8, no tests touched`) but nothing enforces it. | ~40 min |
| 3 | `verify explain` dry-run | Good UX, but only demoable if a contract exists to explain. | ~45 min |
| — | Full `verification-contract/v1` | **Cut.** The research proposing it argues nesting depth is the main cognitive cost, then proposes ~60 lines of YAML. Most of its `execution`/`result`/`evidence` blocks are things the tool should decide, not the developer. | — |

### If #1 is chosen

Add a fourth outcome alongside the existing verdicts, on the same principle that
separates `SAFE` from `UNREACHED`:

```
PASS           all mandatory checks ran and passed
FAIL           a mandatory check ran and did not meet criteria
PARTIAL        some scoped items passed, others did not
CANNOT_VERIFY  a required check could not produce trustworthy evidence
```

The distinction that matters: `go test` failing to compile is `FAIL`; `go test`
failing because modules cannot download is `CANNOT_VERIFY`. Silent pass is wrong,
and so is treating both as ordinary failure.

---

## Block 3 · Submission package — reserve 40 minutes, do not spend it

Three unfinished items, all cheap, in priority order.

### 3.1 Name the branch explicitly — highest value per second left

`main` is **protected** and stale at `3a2a715` (14h old). The push to it was rejected
by the mirror; confirmed, not assumed. A judge who opens the repo root lands on `main`
and sees none of this work.

Fix: state the branch in BUILDATHON.md and in the submission form.

```
Branch:     pivot-work-order        <- ALL work lives here
Final SHA:  <fill at submission>
Tree:       https://entire.io/gh/Adharshd/entire-graph/tree/pivot-work-order
```

### 3.2 Milestone → checkpoint → commit table

Checkpoints are spread across three sessions, so pointing at one session page is not
enough. Give judges the exact mapping:

| Milestone | Checkpoint | Commit |
|---|---|---|
| 1. Initial understanding and architecture | `bb0d72d79dbc` | `2ff8446` |
| 2. Last stable state before noon | `515a95288665` | `cf70f83` |
| 3. Curveball response | _fill_ | _fill_ |
| 4. Final implementation and verification | _fill_ | _fill_ |

Note honestly that `abbf061` and `4a0a219` carry no checkpoint, and why. That note is
already in BUILDATHON.md and reads as rigour rather than a gap.

### 3.3 Fill the Noon Curveball section

Stubbed in BUILDATHON.md. Needs: what the constraint was, what changed, what was
rejected, and the test that proves the new behaviour.

### 3.4 Close out

- [ ] Final semantic diff captured (`entire graph diff --base 4a0a219 --head HEAD`)
- [ ] Final checkpoint present and verified
- [ ] `go test ./internal/cli` green, real output read
- [ ] BUILDATHON.md free of secrets
- [ ] Fallback screenshot/recording available locally
- [ ] Submitted before 15:00

---

## Decisions already made — do not relitigate

| Decision | Reason |
|---|---|
| **Databricks: out** | Free Edition quota exhaustion can disable compute for the rest of the day — a live failure risk during judging, on a dependency the Entire rubric does not score. Reopenable only if the curveball leaves the 13:00–15:00 window genuinely free. |
| **Missing checkpoints on `abbf061`, `4a0a219`: leave them** | Both are pushed. Adding trailers rewrites history and changes every later SHA — breaking 10 working checkpoints to gain 2. Neither is a required milestone. |
| **`main`: unreachable** | Remote rejected the fast-forward: `! [remote rejected] (protected branch)`. Branch + final SHA is the deliverable. |
| **Branch name stays `pivot-work-order`** | Misleading (it carries the whole project, not one feature) but renaming a pushed branch means re-pointing refs the checkpoints depend on. Mitigate with documentation, not surgery. |

---

## Known limitations to state, not hide

Explicitly graded. Say these out loud in the demo:

- **`SAFE` means no path was found in one local snapshot — not proof of non-use.**
  Interface dispatch, reflection and generated code leave no edge to follow. Observed
  concretely: `impact --symbol Route.Match` on gorilla/mux reports 0 callers even
  though `Router.Match` calls it through an interface.
- **`impact` self-reports `Completeness: degraded for Go`** with a
  `W_DATA_FLOW_EVIDENCE_UNMERGED` warning next to its own answer. Show this — a tool
  naming its own incompleteness is the behaviour, not an embarrassment.
- **The loop's count is not sufficient evidence on its own.** `invalidated == 0` is
  satisfied by deletion. The diff is the other half.
- **`cmd/graph-bench` tests fail on this machine** — pre-existing, the local git
  rejects `checkout --end-of-options`. Do not claim a fully green suite; name the
  condition.

---

## Loose ends

Four untracked files in `docs/`, none of them code:

```
PivotMap-Buildathon-Summary.pdf          submission material
pivotmap-submission.html                 source for the PDF
pivotmap-buildathon-alignment.html       internal, "just for us"
loop engg config options ... .txt        research; filename contains spaces
```

Decide before submission which are committed. The PDF is the only one a judge would
look for.
