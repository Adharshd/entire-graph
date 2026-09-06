# Curveball plan — Track 2: Graph is evidence, not an oracle

Started 12:35 IST, 6 Sep 2026. Submission 15:00.

## Card requirements -> where each is satisfied

| # | Requirement | Satisfied by |
|---|---|---|
| 1 | Must not present incomplete relationships as certain | evidence tier printed on every evidence line |
| 2 | Must identify when analysis may be partial | UNVERIFIED tier; file-level parse warnings consulted |
| 3 | Must provide a safe fallback or verification path | `verification_required` + per-item verify command |
| 4 | Existing behaviour for fully resolved code keeps working | CONFIRMED path byte-identical; regression test |
| 5 | Test or fixture representing incomplete analysis | fixture + tests in internal/cli |
| 6 | Use Entire Graph to find evidence consumers | docs/demo/curveball/01..06 (captured pre-edit) |
| 7 | Users and agents can tell the three apart | text tags for users, plan JSON fields for agents |

## Steps

- [x] 0. Reconstruct from checkpoint 9ab0f807a913 (not from memory)
- [x] 1. Graph commands BEFORE any edit, captured to files (01-06, 00)
- [x] 2. Verify in source that confidence/warnings are discarded
- [x] 3. Implement evidence tiers (pivot.go, pivot_roles.go, pivot_plan.go)
- [x] 4. Fixture representing incomplete analysis + tests
- [x] 5. go build ./... && go test ./internal/cli/ -count=1
- [x] 6. Commit + verify Entire-Checkpoint trailer + push
- [~] 7. Assess on kubernetes (dynamic dispatch/generated/reflection at scale)
- [~] 8. Control run on prometheus — DROPPED. kubernetes carries the demo; a
       second large repo adds size, not a new claim.
- [x] 9. Semantic diff 90d1939..HEAD; fill BUILDATHON.md Noon Curveball section
- [x] 10. Final checkpoint + push

## Findings recorded during step 1 (evidence, not assumption)

1. Edges carry `confidence` AND `resolution` ("exact") AND `reason` ("direct call
   expression resolved to same-file symbol"). Tiering can key off the tool's own
   words rather than an invented threshold.
2. This repo reports `completeness_level: "ok"` for Go today, NOT the
   "degraded for Go" recorded pre-curveball. One live partial failure exists:
   E_MINIFIED on a JSON file. The pre-curveball note is stale; not repeated.
3. `pivot --dependency internal/sem` returns 0 invalidated / 0 at-risk /
   205 safe / 80 unreached. `prohibitedMatch` does not resolve internal package
   paths. Recorded as a finding rather than reported as an empty result.
4. The installed plugin has no `pivot` subcommand; it exists only in this
   working tree. All pivot runs use a locally built binary, without the `graph`
   prefix (ENTIRE-TIPS gotcha 11).

## Demo repos (props, not code under change)

kubernetes: 17838 Go files, 4259 generated, 1592 using reflect, 3754 interfaces.
prometheus:   731 Go files,    9 generated,   18 using reflect,  115 interfaces.
Neither has Entire checkpoints; the checkpoint overlay stays demonstrated on this repo.

## Outcome

| Commit | Checkpoint | What it carries |
|---|---|---|
| `d09e6af` | `c8f225bf19ba` | evidence tiers, fixture, 9 tests |
| `400afc5` | `6e867071c6b6` | BUILDATHON.md curveball section, dead helper removed |
| `29b17c5` | `053542bdde85` | requirement-to-answer table |

Suite: `go test ./internal/cli/ -count=1` -> ok, 56.071s, no existing assertion
modified. `cmd/graph-bench` still fails on this machine (local git rejects
`checkout --end-of-options`), pre-existing and unrelated; not a fully green suite
and not claimed as one.

Live result on this repository: five files previously reported SAFE while carrying
`E_PARSE_ERROR` are now UNVERIFIED --
`internal/sem/grammars/{csharp,erlang,fsharp,haskell,perl}/tree_sitter/array.h`.
