#!/usr/bin/env python3
"""Evaluate a pivot plan's invariants against the commit an agent produced.

This is the third role in the PivotMap loop. The first emits a plan, the second
executes it, and this decides whether the result counts -- without asking the
agent anything and without a model in the path. Every check here is a comparison
over data the agent cannot restate: files on disk, `git diff --numstat`, and the
counts from a re-run of the analyser.

The reason it exists is that `invalidated == 0` is satisfied by deleting the code
as well as by fixing it. The plan's `must_not_do` clauses say so in prose, which
binds an agent that reads them; these are the same rules as assertions, which bind
one that does not.

    usage:
      pivot-verify.py --before BEFORE.json --after AFTER.json [--repo PATH]
                      [--rev-range BASE..HEAD]

      --before      the plan captured before the agent started (authoritative)
      --after       a plan produced by re-running pivot on the result
      --repo        repository to check paths in           (default: .)
      --rev-range   git range holding the agent's change   (default: HEAD~1..HEAD)

    exit codes:
      0  PASS           every check ran, every check held, nothing left unchecked
      1  FAIL           a check ran and did not hold
      2  PARTIAL        everything that ran held, but coverage is incomplete
      3  CANNOT_VERIFY  a check could not run at all

PARTIAL is the normal good outcome, not a near-miss. A plan states what it cannot
decide -- whether a rewrite preserved behaviour, whether a heuristic edge holds --
and no check over a diff can close that gap. A verifier that returned PASS while
those questions were open would be making the exact claim this project exists to
refuse. PASS is reserved for the case where the plan itself reports nothing
outstanding, which in practice is rare and should be.

FAIL outranks CANNOT_VERIFY: if one check positively caught a violation, the run
failed, whatever else could not be evaluated.
"""

import argparse
import json
import subprocess
import sys

OK, FAILED, CANNOT_RUN = "ok", "failed", "cannot_run"

PASS, FAIL, PARTIAL, CANNOT_VERIFY = "PASS", "FAIL", "PARTIAL", "CANNOT_VERIFY"
EXIT = {PASS: 0, FAIL: 1, PARTIAL: 2, CANNOT_VERIFY: 3}


class Result:
    def __init__(self, name, status, detail):
        self.name = name
        self.status = status
        self.detail = detail


def git(repo, *args):
    """Run git and return (ok, output). A failure is never silently a pass."""
    try:
        done = subprocess.run(
            ["git", "-C", repo, *args],
            capture_output=True, text=True, timeout=60,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        return False, str(exc)
    if done.returncode != 0:
        return False, (done.stderr or done.stdout).strip()
    return True, done.stdout


def load_plan(path):
    try:
        with open(path) as handle:
            return json.load(handle), None
    except (OSError, json.JSONDecodeError) as exc:
        return None, str(exc)


def check_flags(before, after):
    """The question must be the same question, or the counts are not comparable."""
    want = before.get("validation", {}).get("invariants", {}).get("flags_must_match")
    if want is None:
        return Result("flags_must_match", CANNOT_RUN, "before-plan has no invariants block")
    if after is None:
        return Result("flags_must_match", CANNOT_RUN, "no after-plan supplied")
    got = {
        "dependency": after.get("constraint", {}).get("prohibited"),
        "depth": after.get("graph_tool", {}).get("depth"),
        "exclude_tests": after.get("graph_tool", {}).get("exclude_tests"),
    }
    mismatched = {k: (want.get(k), got.get(k)) for k in want if want.get(k) != got.get(k)}
    if mismatched:
        parts = [f"{k}: plan says {w!r}, re-run used {g!r}" for k, (w, g) in mismatched.items()]
        return Result("flags_must_match", FAILED, "; ".join(parts))
    return Result("flags_must_match", OK, f"re-run used the same question ({got['dependency']}, depth {got['depth']})")


def check_files_exist(repo, invariants):
    """The deletion attack in its simplest form: the violation leaves with the file."""
    required = invariants.get("files_must_still_exist")
    if required is None:
        return Result("files_must_still_exist", CANNOT_RUN, "invariant absent from the plan")
    if not required:
        return Result("files_must_still_exist", OK, "no production violations to protect")
    ok, listing = git(repo, "ls-files", "--")
    if not ok:
        return Result("files_must_still_exist", CANNOT_RUN, f"git ls-files failed: {listing}")
    tracked = set(listing.splitlines())
    missing = [path for path in required if path not in tracked]
    if missing:
        return Result("files_must_still_exist", FAILED,
                      f"{len(missing)} file(s) no longer tracked: {', '.join(missing[:5])}")
    return Result("files_must_still_exist", OK, f"all {len(required)} production violation file(s) still present")


def check_tests_unchanged(repo, rev_range, invariants):
    """A required command that starts passing because its check was removed."""
    protected = invariants.get("paths_must_not_change")
    if protected is None:
        return Result("paths_must_not_change", CANNOT_RUN, "invariant absent from the plan")
    ok, listing = git(repo, "diff", "--name-only", rev_range)
    if not ok:
        return Result("paths_must_not_change", CANNOT_RUN, f"git diff failed: {listing}")
    touched = set(listing.splitlines())

    if not protected:
        # A --exclude-tests run drops test files before classification, so the plan
        # has none to name. That is a gap in the plan, not a reason to stop
        # checking: whether a test file was touched is visible in the diff whether
        # or not the plan enumerated one. Falling back to the conventional paths
        # is exactly the check the mux run did by hand ("no test touched"), and it
        # keeps the common case verifiable instead of permanently CANNOT_VERIFY.
        by_convention = sorted(path for path in touched if is_conventional_test_path(path))
        if by_convention:
            return Result("paths_must_not_change", FAILED,
                          f"the plan named no test paths, but {len(by_convention)} conventional test path(s) "
                          f"changed in this range: {', '.join(by_convention[:5])}")
        return Result("paths_must_not_change", OK,
                      "plan named no test paths (tests excluded before classification); "
                      "no conventional test path changed in the range either")

    violated = sorted(touched.intersection(protected))
    if violated:
        return Result("paths_must_not_change", FAILED,
                      f"{len(violated)} protected test path(s) modified: {', '.join(violated[:5])}")
    return Result("paths_must_not_change", OK, f"none of the {len(protected)} protected test path(s) changed")


def is_conventional_test_path(path):
    """The same shapes --exclude-tests recognises, so the fallback agrees with the
    predicate that produced the empty list in the first place."""
    lowered = path.lower()
    if lowered.endswith("_test.go") or lowered.endswith(".test.ts") or lowered.endswith(".test.js"):
        return True
    if lowered.endswith("_test.py") or lowered.startswith("test_") or "/test_" in lowered:
        return True
    segments = lowered.split("/")
    return any(segment in ("testdata", "test", "tests", "fixtures", "mocks", "__tests__") for segment in segments)


def check_deletion_budget(repo, rev_range, invariants):
    """Replacement is roughly line-neutral. Deletion is not."""
    budget = invariants.get("max_net_lines_deleted")
    if budget is None:
        return Result("max_net_lines_deleted", CANNOT_RUN, "invariant absent from the plan")
    ok, listing = git(repo, "diff", "--numstat", rev_range)
    if not ok:
        return Result("max_net_lines_deleted", CANNOT_RUN, f"git diff --numstat failed: {listing}")
    insertions = deletions = 0
    for line in listing.splitlines():
        fields = line.split("\t")
        if len(fields) < 3 or fields[0] == "-":  # "-" marks a binary file
            continue
        insertions += int(fields[0])
        deletions += int(fields[1])
    net_deleted = deletions - insertions
    detail = f"+{insertions}/-{deletions}, net {net_deleted} removed, budget {budget}"
    if net_deleted > budget:
        return Result("max_net_lines_deleted", FAILED,
                      f"{detail} -- more was removed than a replacement accounts for")
    return Result("max_net_lines_deleted", OK, detail)


def check_expected_after(after, invariants):
    """The count the whole loop turns on, plus the one that guards silencing."""
    expected = invariants.get("expected_after")
    if expected is None:
        return Result("expected_after", CANNOT_RUN, "invariant absent from the plan")
    if after is None:
        return Result("expected_after", CANNOT_RUN, "no after-plan supplied; the count was never re-measured")
    counts = after.get("counts", {})
    invalidated = counts.get("invalidated")
    if invalidated is None:
        return Result("expected_after", CANNOT_RUN, "after-plan carries no invalidated count")
    problems = []
    if invalidated > expected.get("invalidated", 0):
        problems.append(f"invalidated is {invalidated}, expected {expected.get('invalidated', 0)}")
    ceiling = expected.get("unverified_must_not_exceed")
    unverified = counts.get("unverified")
    if ceiling is not None and unverified is not None and unverified > ceiling:
        problems.append(f"unverified rose to {unverified} from {ceiling}")
    if problems:
        return Result("expected_after", FAILED, "; ".join(problems))
    return Result("expected_after", OK, f"invalidated {invalidated}, unverified {unverified}")


def verdict(results, uncheckable):
    if any(r.status == FAILED for r in results):
        return FAIL
    if any(r.status == CANNOT_RUN for r in results):
        return CANNOT_VERIFY
    return PARTIAL if uncheckable else PASS


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--before", required=True, help="plan JSON captured before the agent started")
    parser.add_argument("--after", help="plan JSON from re-running pivot on the result")
    parser.add_argument("--repo", default=".", help="repository to check paths in")
    parser.add_argument("--rev-range", default="HEAD~1..HEAD", help="git range holding the change")
    args = parser.parse_args()

    before, error = load_plan(args.before)
    if error:
        print(f"cannot read the before-plan: {error}")
        print(f"\nVERDICT: {CANNOT_VERIFY}")
        return EXIT[CANNOT_VERIFY]

    after = None
    after_error = None
    if args.after:
        after, after_error = load_plan(args.after)

    invariants = before.get("validation", {}).get("invariants", {})
    uncheckable = invariants.get("not_mechanically_checkable", [])

    results = [
        check_flags(before, after),
        check_files_exist(args.repo, invariants),
        check_tests_unchanged(args.repo, args.rev_range, invariants),
        check_deletion_budget(args.repo, args.rev_range, invariants),
        check_expected_after(after, invariants),
    ]

    print(f"pivot-verify: {before.get('plan_id', '(no plan id)')}")
    print(f"  repo {args.repo} | range {args.rev_range}")
    if after_error:
        print(f"  after-plan could not be read: {after_error}")
    print()
    marks = {OK: "ok  ", FAILED: "FAIL", CANNOT_RUN: "??  "}
    for result in results:
        print(f"  [{marks[result.status]}] {result.name}")
        print(f"           {result.detail}")

    if uncheckable:
        print(f"\n  Not mechanically checkable ({len(uncheckable)}) -- these need a human or a test:")
        for item in uncheckable:
            print(f"    - {item}")

    final = verdict(results, uncheckable)
    print(f"\nVERDICT: {final}")
    if final == PARTIAL:
        print("  Every check that ran held. Coverage is incomplete by the plan's own account,")
        print("  which is why this is not a PASS. Read the list above before shipping.")
    elif final == CANNOT_VERIFY:
        print("  At least one check could not run. That is not a pass; it is an absence of")
        print("  evidence, and the loop must not treat it as one.")
    return EXIT[final]


if __name__ == "__main__":
    sys.exit(main())
