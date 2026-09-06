# Executing a PivotMap plan — brief for a fresh agent session

You have been handed `pivot-plan/v1` JSON, produced by:

    entire graph pivot --repo . --dependency <prohibited> --format plan

A constraint changed after this code was written. The plan is the bounded list of work
that constraint creates. It was produced by rules over a local parsed code graph — no
model wrote a verdict, so nothing in it is a guess about intent, and nothing in it is
proof about behaviour either. Read it as evidence collected by a tool that cannot run
the code.

You are starting with no memory of why any of this was built. That is the normal case
this plan exists for, and it is exactly why you must not improvise scope.

## The rule that matters most

**Only `INVALIDATED` items are edit targets.** Everything else is reachable, not wrong.

The common failure here is treating the whole blast radius as a to-do list: an agent
sees 300 at-risk files, starts "fixing" them, and rewrites working code that was never
in violation. `AT-RISK` means *this file sits on top of something being replaced* — you
verify it after the root fix lands, you do not pre-emptively edit it.

The plan already encodes this. Obey `action`, never infer work from `verdict` alone:

| `verdict`     | `action`                | what you actually do                                  |
|---------------|-------------------------|-------------------------------------------------------|
| `INVALIDATED` | `REPLACE_OR_REMOVE`     | Edit. This file breaks the constraint.                 |
| `AT-RISK`     | `VERIFY_AFTER_ROOT_FIX` | Do not edit first. Re-check after its root is fixed.   |
| `SAFE`        | `NO_ACTION`             | Leave it alone. Do not open it "just to be sure".      |
| `UNREACHED`   | `MANUAL_REVIEW`         | Do not edit. Report it for a human.                    |

If you believe an `AT-RISK` file must change, say so in your report with the reason.
Do not silently promote it to an edit.

## Order of work

Strict priority order: **all P0, then P1, then P2.** Do not start P1 while a P0 item is
unresolved, and do not batch unrelated files into one change.

P0 items are the direct violations. They are the only work that removes the constraint
breach; everything after them is confirming the repair held.

## The loop, per work item

1. **Read `evidence[]` first.** Each entry names a `relation`, a `to`, a `line`, and a
   `confidence`. That is the specific fact that put this file in the plan.
2. **Open the cited source at that line before deciding anything.** The graph reports a
   parsed edge, not a behaviour. Confirm the violation is really there and really does
   what the constraint prohibits.
3. **If the evidence does not hold up, stop and record that.** A false positive is a
   legitimate outcome and is more useful than a defensive edit. Do not "fix" code to
   make a wrong finding look right.
4. **Make the smallest complete change** that satisfies `acceptance_criteria[]` for that
   item, following `agent_instructions[]`.
5. **Record the outcome on the item** before moving on: `fixed`, `no_change_needed`,
   `blocked`, or `disputed`, each with a one-line reason.

## What this plan cannot tell you

`known_blind_spots` in the plan is not boilerplate. Static analysis reads source without
running it, so these leave no edge to follow and no verdict behind them:

- calls dispatched through an interface
- reflection, and anything assembled from a string at runtime
- generated code, and build-tag-gated files not in this configuration

The consequence, stated plainly: **`SAFE` means no path was found in one local snapshot.
It is not proof of non-use.** If you have direct source evidence that a `SAFE` file
violates the constraint, that evidence wins over the plan. Report the discrepancy rather
than quietly acting on it.

## Validation

Run every command in `validation.required_commands[]` after your edits. Report the real
output, including failures.

The plan is checkable: re-running the same local analysis after your work should show the
violation count fall. That re-run is not your claim to make — it is run independently
against your committed result.

## What you must not claim

`execution_contract.must_not_claim[]` is binding. In particular, never state that:

- tests pass, unless you executed them and read the output
- a file is unaffected, when what you mean is that no edge was found
- the constraint is fully resolved, when only some P0 items are done

If you could not run validation, say so explicitly and say why. An honest blocked item is
worth more than a confident false one — the whole point of this plan is that a human can
check every claim in it against evidence.

## What to hand back

For each work item: its `id`, the outcome, what you changed, and the validation output
that supports it. List separately anything you disputed, anything blocked, and any
blind-spot finding a human needs to look at.
