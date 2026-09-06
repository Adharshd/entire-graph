#!/usr/bin/env python3
"""Expose PivotMap to any coding agent over the Model Context Protocol.

`entire mcp` already runs an MCP stdio server: read-only, newline-delimited
JSON-RPC 2.0, exposing agent-help and status so an agent without a hook can
discover Entire. It deliberately stops there and does not expose the graph.

This is the complementary surface -- analysis and verification -- in the same
shape, so the two read as one family rather than two conventions. Read-only,
stdio, no network, no model. It shells out to a locally built entire-graph
binary and returns what that binary said.

The point of the exercise is narrower than "add an integration". PivotMap's
whole claim is that a graph edge is evidence rather than an oracle, and that
claim only survives the trip to another tool if the uncertainty travels with the
answer. An agent in someone else's editor will never read this repository's
documentation. So the tool descriptions below carry the rule themselves, and
every response repeats it in the text rather than burying it in a field the
caller may not open.

There are two surfaces here, for two audiences, over the same analysis.

  developer   pivot, pivot_verify, impact, capabilities
              Speaks the graph's own vocabulary. Assumes the reader knows that
              SAFE means "no path found" and is sceptical accordingly.

  product     assess_product_constraint_change, explain_product_impact,
              prepare_engineering_discovery_brief
              For whoever RECEIVED the constraint -- a product manager, a
              designer, legal, compliance. They are usually the ones who cannot
              answer "what does this cost us" without an engineer translating,
              and that translation is the expensive step. These tools take the
              constraint in business language, refuse to guess which dependency
              is meant when it is unclear, and return a decision brief rather
              than a file listing. They never emit SAFE, AT-RISK or a bare
              confidence number, because a non-specialist reads a coverage gap
              as a clean result. They never estimate effort, because structural
              analysis cannot see what determines it.

Neither surface can change code. Both are read-only by construction.

MCP host configuration -- Claude Code, Claude Desktop and Cursor all accept this
shape. Point ENTIRE_GRAPH_BIN at a binary built from this working tree, because
the pivot subcommand does not exist in a released plugin:

    {
      "mcpServers": {
        "entire-graph-pivot": {
          "command": "python3",
          "args": ["/absolute/path/to/entire-graph/scripts/pivot-mcp.py"],
          "env": {
            "ENTIRE_GRAPH_BIN": "/absolute/path/to/entire-graph/entire-graph"
          }
        }
      }
    }

Build the binary first (CGO is required for tree-sitter):

    go build -o ./entire-graph ./cmd/entire-graph

Claude Code can also register it without editing a file:

    claude mcp add entire-graph-pivot \
      --env ENTIRE_GRAPH_BIN=/absolute/path/to/entire-graph/entire-graph \
      -- python3 /absolute/path/to/entire-graph/scripts/pivot-mcp.py

Diagnostics go to stderr. stdout carries JSON-RPC frames and nothing else: a
stray print there corrupts the stream and the host drops the server with no
useful error.
"""

import json
import os
import subprocess
import sys
import tempfile

SERVER_NAME = "entire-graph-pivot"
SERVER_VERSION = "0.1.0"

# The protocol version we answer with when a client does not state one. We
# implement a version-agnostic subset -- initialize, tools/list, tools/call --
# so when the client names a version we echo it back rather than forcing ours
# and being hung up on. See _initialize.
DEFAULT_PROTOCOL_VERSION = "2025-06-18"

# Generous, because a cold parse of a large repository is minutes, not seconds:
# kubernetes took 608s. A timeout that fires early would report CANNOT_VERIFY
# for a healthy run, which is the one wrong answer this project cares about.
TIMEOUT_SECONDS = int(os.environ.get("PIVOT_MCP_TIMEOUT", "1200"))

VERIFY_SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pivot-verify.py")


def log(message):
    """Diagnostics to stderr. stdout is reserved for the protocol."""
    print(f"[{SERVER_NAME}] {message}", file=sys.stderr, flush=True)


# --------------------------------------------------------------------------
# the binary
# --------------------------------------------------------------------------


def resolve_binary():
    """Locate the entire-graph binary, or say precisely why we cannot.

    Returning a reason rather than guessing matters more here than it looks.
    `pivot` is not in any released plugin -- it exists only in this working
    tree -- so the failure a caller will actually hit is a binary that runs but
    does not know the subcommand. Naming that beats a bare "not found".
    """
    candidate = os.environ.get("ENTIRE_GRAPH_BIN") or "./entire-graph"
    path = os.path.abspath(candidate)
    if not os.path.exists(path):
        return None, (
            f"entire-graph binary not found at {path}. "
            "Set ENTIRE_GRAPH_BIN to a binary built from this working tree, or build one with "
            "`go build -o ./entire-graph ./cmd/entire-graph` (CGO required for tree-sitter). "
            "A released plugin will not do: the pivot subcommand exists only in this fork."
        )
    if not os.access(path, os.X_OK):
        return None, f"entire-graph binary at {path} is not executable."
    return path, None


def run(argv, cwd=None):
    """Run a subprocess and return (ok, stdout, detail).

    A non-zero exit is never quietly turned into an empty success. The caller
    gets the command and the stderr, because an agent that receives an empty
    result will usually assume the answer was 'nothing'.
    """
    printable = " ".join(argv)
    try:
        completed = subprocess.run(
            argv,
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=TIMEOUT_SECONDS,
        )
    except FileNotFoundError:
        return False, "", f"command not found: {argv[0]}"
    except subprocess.TimeoutExpired:
        return False, "", (
            f"command exceeded {TIMEOUT_SECONDS}s and was killed: {printable}. "
            "A cold parse of a large repository is slow; raise PIVOT_MCP_TIMEOUT if this was healthy."
        )
    except OSError as exc:
        return False, "", f"could not run {printable}: {exc}"

    if completed.returncode != 0:
        stderr = (completed.stderr or "").strip() or "(no stderr)"
        return False, completed.stdout, f"`{printable}` exited {completed.returncode}: {stderr}"
    return True, completed.stdout, ""


# --------------------------------------------------------------------------
# carrying the uncertainty forward
# --------------------------------------------------------------------------


def uncertainty_header(plan):
    """Summarise what this plan does not know, before its content is read.

    An agent decides how far to trust an answer before it acts on it, not after,
    so this goes at the top. It restates in prose what the plan carries as
    fields, because a caller in another tool may render only the first lines of
    a tool result.
    """
    counts = plan.get("counts", {}) or {}
    items = plan.get("work_items", []) or []

    tiers = {}
    needs_verification = 0
    for item in items:
        quality = item.get("evidence_quality") or "(none)"
        tiers[quality] = tiers.get(quality, 0) + 1
        if item.get("verification_required"):
            needs_verification += 1

    lines = [
        "HOW FAR TO TRUST THIS ANSWER",
        f"  {counts.get('invalidated', 0)} invalidated, {counts.get('at_risk', 0)} at-risk, "
        f"{counts.get('safe', 0)} safe, {counts.get('unverified', 0)} unverified, "
        f"{counts.get('unreached', 0)} unreached",
    ]

    graded = ", ".join(
        f"{tiers[tier]} {tier}" for tier in ("CONFIRMED", "HEURISTIC", "UNVERIFIED") if tier in tiers
    )
    if graded:
        lines.append(f"  evidence: {graded}")

    if needs_verification:
        lines.append(
            f"  {needs_verification} work item(s) carry verification_required=true. Their evidence is "
            "not CONFIRMED: open the cited source line, or run a test, BEFORE acting on the item or "
            "reporting it done."
        )
    else:
        lines.append("  No work item requires source verification: every graded edge is CONFIRMED.")

    if counts.get("unverified"):
        lines.append(
            f"  {counts['unverified']} file(s) are UNVERIFIED -- the parser could not read them, so "
            "'no dependency path found' is unknown rather than established. They must not be counted "
            "towards the constraint being satisfied."
        )
    if counts.get("unreached"):
        lines.append(
            f"  {counts['unreached']} file(s) are UNREACHED -- an inventory-only language with no "
            "relations parsed. A SAFE verdict was never available for these."
        )

    blind_spots = plan.get("known_blind_spots", []) or []
    if blind_spots:
        lines.append(f"  {len(blind_spots)} declared blind spot(s), listed in the plan before its work items.")

    lines.append(
        "  SAFE means no path was found in this snapshot, never that none exists. Calls through "
        "interfaces, reflection and generated code leave no edge to follow."
    )
    return "\n".join(lines)


def text_uncertainty_note(output):
    """The same service for text output, which has no fields to summarise."""
    notes = []
    if "[HEURISTIC]" in output or "[UNVERIFIED]" in output:
        notes.append(
            "This report contains evidence tagged HEURISTIC or UNVERIFIED. Those edges were inferred "
            "or came from a file that did not parse. Confirm them against source before acting."
        )
    if "VERIFY:" in output:
        notes.append("Lines beginning VERIFY: mark verdicts that must not be acted on from the graph alone.")
    if not notes:
        return ""
    return "HOW FAR TO TRUST THIS ANSWER\n  " + "\n  ".join(notes)


# --------------------------------------------------------------------------
# tools
# --------------------------------------------------------------------------


def pivot_argv(binary, repo, dependency, depth, exclude_tests, output_format):
    argv = [binary, "pivot", "--repo", repo]
    for item in dependency:
        argv += ["--dependency", item]
    argv += ["--depth", str(depth), "--format", output_format]
    if exclude_tests:
        argv.append("--exclude-tests")
    return argv


def tool_pivot(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    dependency = args.get("dependency")
    if not repo:
        return "pivot requires `repo`: the path to the repository to analyse.", True
    if not dependency:
        return (
            "pivot requires `dependency`: one or more package names that are now prohibited. "
            "It takes a package name, not a sentence -- turning a requirement into a package name "
            "is the caller's job and stays outside this tool on purpose.",
            True,
        )
    if isinstance(dependency, str):
        dependency = [dependency]

    depth = args.get("depth", 2)
    exclude_tests = bool(args.get("exclude_tests", False))
    output_format = args.get("format", "plan")
    if output_format not in ("text", "json", "plan"):
        return f"unknown format {output_format!r}: expected text, json or plan.", True

    argv = pivot_argv(binary, repo, dependency, depth, exclude_tests, output_format)
    ok, stdout, detail = run(argv)
    if not ok:
        if "unknown command" in detail:
            detail += (
                "\n\nThe binary does not have the pivot subcommand. It is only in this fork's working "
                "tree; rebuild with `go build -o ./entire-graph ./cmd/entire-graph` and point "
                "ENTIRE_GRAPH_BIN at the result."
            )
        return detail, True

    if output_format == "plan":
        try:
            plan = json.loads(stdout)
        except json.JSONDecodeError as exc:
            return f"pivot returned output that is not valid JSON ({exc}). Raw output follows:\n\n{stdout}", True
        return f"{uncertainty_header(plan)}\n\n{json.dumps(plan, indent=2)}", False

    note = text_uncertainty_note(stdout)
    return f"{note}\n\n{stdout}" if note else stdout, False


def tool_pivot_verify(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    before_plan = args.get("before_plan")
    rev_range = args.get("range")
    if not repo or not before_plan or not rev_range:
        return (
            "pivot_verify requires `repo`, `before_plan` (the plan captured BEFORE the agent started) "
            "and `range` (the git range holding the change, e.g. HEAD~1..HEAD).",
            True,
        )
    if not os.path.exists(before_plan):
        return f"before_plan not found at {before_plan}. Without the baseline nothing can be compared.", True

    after_plan = args.get("after_plan")
    temporary = None

    if not after_plan:
        # Re-run pivot to produce the after-state. It has to be the SAME question
        # or the two counts are not comparable, so the flags come out of the
        # before-plan rather than from anything the agent supplies.
        try:
            with open(before_plan, "r", encoding="utf-8") as handle:
                before = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            return f"could not read before_plan at {before_plan}: {exc}", True

        invariants = (before.get("validation", {}) or {}).get("invariants", {}) or {}
        flags = invariants.get("flags_must_match") or {}
        dependency = flags.get("dependency") or (before.get("constraint", {}) or {}).get("prohibited")
        tool_block = before.get("graph_tool", {}) or {}
        depth = flags.get("depth", tool_block.get("depth", 2))
        exclude_tests = flags.get("exclude_tests", tool_block.get("exclude_tests", False))

        if not dependency:
            return (
                f"before_plan at {before_plan} names no prohibited dependency, so the same question "
                "cannot be asked again. Supply after_plan explicitly.",
                True,
            )

        argv = pivot_argv(binary, repo, dependency, depth, exclude_tests, "plan")
        ok, stdout, detail = run(argv)
        if not ok:
            return f"could not produce the after-plan, so the result cannot be graded:\n{detail}", True

        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        handle.write(stdout)
        handle.close()
        temporary = handle.name
        after_plan = temporary

    try:
        argv = [
            sys.executable,
            VERIFY_SCRIPT,
            "--before", before_plan,
            "--after", after_plan,
            "--repo", repo,
            "--rev-range", rev_range,
        ]
        printable = " ".join(argv)
        try:
            completed = subprocess.run(argv, capture_output=True, text=True, timeout=TIMEOUT_SECONDS)
        except (OSError, subprocess.TimeoutExpired) as exc:
            return f"could not run the verifier (`{printable}`): {exc}", True

        # The verifier signals its verdict through the exit code, so a non-zero
        # status is the answer, not a failure. Only a code outside the four it
        # documents means something actually went wrong.
        verdicts = {0: "PASS", 1: "FAIL", 2: "PARTIAL", 3: "CANNOT_VERIFY"}
        verdict = verdicts.get(completed.returncode)
        if verdict is None:
            stderr = (completed.stderr or "").strip() or "(no stderr)"
            return f"`{printable}` exited {completed.returncode}, which is not a verdict: {stderr}", True

        guidance = {
            "PASS": "Every check ran, every check held, and the plan reported nothing outstanding. Rare by design.",
            "FAIL": "A check ran and did not hold. The change does not satisfy the plan, whatever the counts say.",
            "PARTIAL": (
                "This is the normal good outcome, not a near-miss. Every check that ran held; coverage is "
                "incomplete because whether behaviour was preserved is not decidable from a diff. Read the "
                "not-mechanically-checkable list before shipping."
            ),
            "CANNOT_VERIFY": (
                "A check could not run. This is an ABSENCE OF EVIDENCE and must never be reported as a pass."
            ),
        }[verdict]

        header = f"VERDICT: {verdict} (exit {completed.returncode})\n  {guidance}"
        if temporary:
            header += "\n  The after-plan was produced by re-running pivot with the before-plan's own flags."
        return f"{header}\n\n{completed.stdout}", False
    finally:
        if temporary:
            try:
                os.unlink(temporary)
            except OSError:
                pass


def tool_impact(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    symbol = args.get("symbol")
    if not repo or not symbol:
        return "impact requires `repo` and `symbol`.", True

    argv = [binary, "impact", "--repo", repo, "--symbol", symbol, "--depth", str(args.get("depth", 2))]
    if args.get("file"):
        argv += ["--file", args["file"]]

    ok, stdout, detail = run(argv)
    if not ok:
        return detail, True

    note = (
        "HOW FAR TO TRUST THIS ANSWER\n"
        "  Read the Completeness line below: it is the parser's own account of what it could not read, "
        "and it bounds everything else in this answer.\n"
        "  A relation absent here is not proof of no relation. Calls through interfaces, reflection and "
        "generated code leave no edge, so 0 callers can mean 'none found', not 'none exist'.\n"
    )
    if "definitions" in stdout.lower() and "--file" in stdout:
        note += "  The symbol name looks ambiguous; rerun with `file` to pick one definition.\n"
    return f"{note}\n{stdout}", False


def tool_capabilities(_args):
    binary, error = resolve_binary()
    if error:
        return error, True

    ok, stdout, detail = run([binary, "capabilities", "--json"])
    if not ok:
        return detail, True

    note = (
        "HOW FAR TO TRUST THIS ANSWER\n"
        "  Languages listed as inventory-only are parsed for structure but NOT for call or import "
        "relations. No relation is ever found in them, so pivot reports their files UNREACHED rather "
        "than SAFE -- a SAFE verdict on such a file would assert an absence nobody checked.\n"
        "  Check this before trusting any relation-based answer about a language you have not confirmed "
        "is semantic.\n"
    )
    return f"{note}\n{stdout}", False


# --------------------------------------------------------------------------
# the product surface: the same evidence, for someone who does not read code
# --------------------------------------------------------------------------
#
# The four tools above are developer-shaped. They answer "which file imports
# this package", which is not a question a product manager, a designer or a
# lawyer ever asks. Those people are usually the ones who ARRIVE with the
# constraint -- legal drops a vendor, compliance bans a library, a requirement
# changes -- and they cannot get from there to "what does this cost us" without
# an engineer translating for them. The translation is the expensive step, not
# the ticket.
#
# So this surface answers the business question and never hands back the
# engineering artefact: no file paths as the headline, no call-graph vocabulary,
# and no effort estimate, because a structural analysis cannot produce one
# honestly and a wrong one destroys trust faster than no answer.
#
# The vocabulary is the load-bearing part. A developer reading SAFE is sceptical
# by training; a non-developer reads it as proof. So the word is never emitted
# here. "No impact found in analyzed code" carries its own coverage caveat in
# the same breath, which "safe" does not.

PRODUCT_LABELS = {
    "INVALIDATED": "Directly affected",
    "AT-RISK": "Needs engineering validation",
    "SAFE": "No impact found in analyzed code",
    "UNVERIFIED": "Not assessed - the analyzer could not read these files",
    "UNREACHED": "Not assessed",
}

# Words that carry no signal about which dependency is meant. Without this,
# "we can no longer use X for customer data" matches every package containing
# "data", and the tool would offer a shortlist that is really a directory
# listing.
_STOPWORDS = {
    "the", "and", "for", "with", "from", "that", "this", "our", "are", "can",
    "cannot", "can't", "cant", "not", "any", "all", "must", "should", "would",
    "will", "shall", "may", "longer", "anymore", "use", "using", "used", "uses",
    "stop", "drop", "dropping", "remove", "removing", "removed", "replace",
    "replacing", "ban", "banned", "banning", "prohibit", "prohibited", "forbid",
    "forbidden", "allow", "allowed", "says", "say", "said", "need", "needs",
    "want", "wants", "legal", "compliance", "policy", "team", "vendor",
    "vendors", "library", "libraries", "package", "packages", "dependency",
    "dependencies", "code", "codebase", "system", "systems", "app", "apps",
    "application", "product", "products", "feature", "features", "service",
    "services", "customer", "customers", "user", "users", "data", "info",
    "information", "through", "anything", "everything", "something", "have",
    "has", "had", "into", "onto", "over", "under", "about", "because", "since",
    "when", "where", "which", "what", "why", "how", "who", "whom", "been",
    "being", "was", "were", "our", "their", "there", "here", "more", "less",
    "new", "old", "next", "last", "first", "please", "could", "might",
}


def _tokens(text):
    """Meaningful words from a plain-language constraint.

    Short words are dropped as well as stopwords: two-letter fragments match
    almost every import path and would turn a shortlist into noise.
    """
    word = []
    words = []
    for char in text.lower():
        if char.isalnum():
            word.append(char)
        else:
            if word:
                words.append("".join(word))
            word = []
    if word:
        words.append("".join(word))
    return [w for w in words if len(w) >= 3 and w not in _STOPWORDS]


def harvest_dependency_candidates(binary, repo):
    """Every external package this repository actually declares an import of.

    Matching against a real inventory rather than against the model's guess is
    the whole point. A package name invented from a vendor's marketing name is
    exactly the failure this tool exists to avoid: it would produce a confident
    impact report about a dependency that is not in the code.
    """
    ok, stdout, detail = run(
        [binary, "edges", "--repo", repo, "--relation", "IMPORTS", "--format", "ndjson"]
    )
    if not ok:
        return None, detail

    seen = set()
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if record.get("record_type") != "relation":
            continue
        target = record.get("to_id") or ""
        if target.startswith("external:import:"):
            name = target[len("external:import:"):]
            if name:
                seen.add(name)
    return sorted(seen), None


def match_dependency_candidates(description, candidates):
    """Candidate technical targets for a business-language description.

    Returns (strong, weak). A strong match is a whole path segment: "stripe"
    against github.com/stripe/stripe-go. A weak match is a segment that merely
    contains the word, which is worth offering and never worth assuming.
    """
    tokens = _tokens(description)
    if not tokens:
        return [], []

    strong, weak = [], []
    for candidate in candidates:
        segments = []
        for part in candidate.replace("-", "/").replace("_", "/").replace(".", "/").split("/"):
            if part:
                segments.append(part.lower())
        if not segments:
            continue
        if any(token in segments for token in tokens):
            strong.append(candidate)
            continue
        if any(len(token) >= 4 and any(token in segment for segment in segments) for token in tokens):
            weak.append(candidate)
    return strong, weak


def product_areas(work_items):
    """Group affected files by the honest route available: their directory.

    This is deliberately not called a capability map. A real one needs ownership
    metadata -- which team owns which user-facing journey -- that a repository
    does not carry, and inventing it would be the same overclaim this whole
    project exists to remove.
    """
    areas = {}
    for item in work_items:
        path = item.get("path") or ""
        parts = [p for p in path.split("/") if p]
        if not parts:
            continue
        area = "/".join(parts[:2]) if len(parts) > 2 else (parts[0] if len(parts) == 1 else "/".join(parts[:1]))
        bucket = areas.setdefault(area, {"direct": 0, "validate": 0, "not_assessed": 0, "paths": []})
        verdict = item.get("verdict")
        if verdict == "INVALIDATED":
            bucket["direct"] += 1
            if len(bucket["paths"]) < 5:
                bucket["paths"].append(path)
        elif verdict == "AT-RISK":
            bucket["validate"] += 1
        elif verdict in ("UNVERIFIED", "UNREACHED"):
            bucket["not_assessed"] += 1
    return areas


def decision_class(counts, direct_areas, production_direct):
    """Scope shape, never an effort estimate.

    A structural analysis can see how much code is involved and how spread out
    it is. It cannot see business ambiguity, migration semantics, vendor
    behaviour, rollout, team familiarity or review latency -- which is most of
    what determines how long anything takes. So the answer is a class and the
    counts that produced it, and the reader does the judgement.
    """
    direct = counts.get("invalidated", 0)
    validate = counts.get("at_risk", 0)
    not_assessed = counts.get("unverified", 0) + counts.get("unreached", 0)
    total = max(counts.get("total", 0), 1)
    spread = len(direct_areas)

    if direct == 0 and not_assessed == 0:
        return (
            "No impact found in analyzed code",
            f"nothing in the {total} analyzed files reaches it, and every file was readable",
        )
    if direct == 0:
        return (
            "Discovery required",
            f"nothing analyzed reaches it, but {not_assessed} file(s) could not be assessed at all, "
            "so the absence is unconfirmed rather than established",
        )
    if counts.get("unverified", 0) > 0 or not_assessed > total * 0.25:
        return (
            "Discovery required",
            f"{direct} direct use(s) found, but {not_assessed} file(s) were not assessed "
            f"({not_assessed * 100 // total}% of the analyzed set), so the picture is incomplete",
        )
    if spread >= 6 and direct >= 20:
        return (
            "Architectural decision required",
            f"{direct} direct uses spread across {spread} areas: this is a platform boundary, "
            "not an implementation detail",
        )
    if spread >= 4:
        return (
            "Distributed migration",
            f"{direct} direct use(s) across {spread} separate areas, so the work cannot be done "
            "in one place",
        )
    if validate >= 5 * max(production_direct, 1):
        return (
            "Concentrated but risky",
            f"only {direct} direct use(s) in {spread} area(s), but {validate} other place(s) "
            "depend on them and need re-checking",
        )
    return (
        "Small bounded change",
        f"{direct} direct use(s) in {spread} area(s), with {validate} place(s) needing "
        "re-checking afterwards",
    )


def _scope_flags(scope):
    """--exclude-tests, plus the question the choice leaves open."""
    if scope == "production_only":
        return True, (
            "You asked about production code only, so test and fixture files were removed before "
            "the analysis ran. They carry no finding here even if they use it."
        )
    if scope == "all_code":
        return False, "You asked about all code, so test and fixture files are included in the counts below."
    return False, (
        "You did not say whether the policy covers test-only use, so ALL code was analyzed and test "
        "files are included in the counts. This usually inflates the apparent size of the work: test "
        "usage is often a single shared helper. Say `production_only` to see the shipped-code answer."
    )


def _confidence_block(plan, counts, coverage_note):
    """Three separate questions a single confidence number would conflate.

    Evidence strength, analysis coverage and product interpretation are
    different claims with different reliability, and collapsing them into one
    percentage invites a reader to treat the weakest as if it were the
    strongest.
    """
    items = plan.get("work_items", []) or []
    graded = {}
    for item in items:
        quality = item.get("evidence_quality")
        if quality:
            graded[quality] = graded.get(quality, 0) + 1
    confirmed = graded.get("CONFIRMED", 0)
    inferred = graded.get("HEURISTIC", 0)
    unreadable = graded.get("UNVERIFIED", 0)

    if confirmed and not inferred and not unreadable:
        strength = (
            f"Strong. All {confirmed} connection(s) were read directly from source -- an import "
            "statement or a call resolved to its definition."
        )
    elif confirmed >= max(inferred, 1):
        strength = (
            f"Mixed, mostly direct. {confirmed} connection(s) were read directly from source; "
            f"{inferred} were inferred from a matching name and could point at the wrong place."
        )
    elif inferred:
        strength = (
            f"Mostly inferred. Only {confirmed} connection(s) were read directly from source, while "
            f"{inferred} were matched by name. An engineer should confirm those before you rely on them."
        )
    else:
        strength = "No graded connections were found, so there is no direct evidence to weigh."
    if unreadable:
        strength += f" A further {unreadable} came from files the analyzer could not fully read."

    not_assessed = counts.get("unverified", 0) + counts.get("unreached", 0)
    if not_assessed == 0:
        coverage = "Complete for the analyzed set. Every file in scope was readable."
    else:
        coverage = (
            f"Partial. {not_assessed} file(s) could not be assessed -- either written in a language "
            "this analyzer reads for structure but not for connections, or files it failed to parse. "
            "Nothing was found in them because nothing could be looked for."
        )
    coverage += (
        " Separately, connections made at run time -- through plugin interfaces, reflection, or "
        "machine-generated code -- leave no trace for any static analysis to follow. This bounds "
        "every statement above."
    )
    if coverage_note:
        coverage += f" {coverage_note}"

    interpretation = (
        "Weakest of the three, and the one to confirm with an owner. The areas named in this brief are "
        "directories, "
        "not user-facing capabilities. Whether a directory corresponds to a customer journey is a "
        "judgement this tool cannot make, because the repository carries no ownership metadata."
    )
    return strength, coverage, interpretation


def _next_decision(decision_needed, klass, counts):
    """What the reader has to decide, shaped by what they said they needed."""
    direct = counts.get("invalidated", 0)
    not_assessed = counts.get("unverified", 0) + counts.get("unreached", 0)

    if decision_needed == "feasibility":
        if klass == "No impact found in analyzed code":
            return (
                "Nothing in the analyzed code uses it, so nothing blocks accepting the constraint "
                "on these grounds. Confirm the run-time cases below before treating that as final."
            )
        return (
            f"It is feasible in the sense that the work is bounded and named: {direct} place(s) must "
            "change. Whether it is affordable is a scheduling question this analysis cannot answer."
        )
    if decision_needed == "risk_review":
        return (
            "Focus review on the areas with direct uses listed below, and treat the not-assessed "
            f"count ({not_assessed}) as the real risk: those are places nobody has looked."
        )
    if decision_needed == "prioritization":
        return (
            "Use the scope class and the spread across areas to rank this against other work. "
            "Do not convert either into a delivery date; nothing here supports one."
        )
    if decision_needed == "engineering_discovery":
        return (
            "Hand this to engineering as a discovery task. `prepare_engineering_discovery_brief` "
            "turns it into a bounded brief with the open questions and the technical work order."
        )
    return (
        "Decide one thing before anything else: does the constraint cover the whole codebase or "
        "only shipped production code? That single answer changes the size of this materially. "
        "After that, choose between funding a short engineering discovery and altering the "
        "requirement."
    )


def _run_product_pivot(binary, repo, dependency, scope):
    """Shared: run the analysis and hand back the plan."""
    exclude_tests, scope_note = _scope_flags(scope)
    argv = pivot_argv(binary, repo, [dependency], 2, exclude_tests, "plan")
    ok, stdout, detail = run(argv)
    if not ok:
        return None, None, detail
    try:
        plan = json.loads(stdout)
    except json.JSONDecodeError as exc:
        return None, None, f"the analysis returned output that could not be read ({exc})."
    return plan, scope_note, None


def tool_assess_product_constraint_change(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    description = args.get("change_description")
    if not repo:
        return "assess_product_constraint_change requires `repo`: the repository to analyse.", True
    if not description or not str(description).strip():
        return (
            "assess_product_constraint_change requires `change_description`: the constraint in plain "
            "language, as the person who received it would say it. For example: \"Legal says we can no "
            "longer use Vendor X for customer data.\" Do not translate it into a package name first -- "
            "working out which package is meant, and refusing to guess when it is unclear, is what this "
            "tool is for.",
            True,
        )
    description = str(description)

    scope = args.get("scope", "unspecified")
    if scope not in ("production_only", "all_code", "unspecified"):
        return f"unknown scope {scope!r}: expected production_only, all_code or unspecified.", True
    decision_needed = args.get("decision_needed", "scope")

    candidates, detail = harvest_dependency_candidates(binary, repo)
    if candidates is None:
        return f"could not read this repository's dependencies, so nothing can be assessed:\n{detail}", True

    strong, weak = match_dependency_candidates(description, candidates)
    shortlist = strong or weak

    if not shortlist:
        return (
            "NO TECHNICAL TARGET IDENTIFIED -- no impact claim will be made.\n\n"
            f'Your description: "{description}"\n\n'
            f"None of the words in it match any of the {len(candidates)} external dependencies this "
            "repository actually declares. That is a finding, not a failure: it may mean the thing "
            "you named is not used here at all, or that it is used under a different name than the "
            "one the business uses for it.\n\n"
            "This tool will not guess a package name. A confident report about a dependency that is "
            "not in the code is worse than no report.\n\n"
            "WHAT WOULD UNBLOCK THIS\n"
            "  - The vendor's actual package or module name, if someone knows it.\n"
            "  - Or ask an engineer which dependency implements the capability you mean.\n",
            False,
        )

    if len(shortlist) > 1:
        shown = shortlist[:12]
        lines = [
            "MULTIPLE POSSIBLE TARGETS -- no impact claim will be made until one is confirmed.",
            "",
            f'Your description: "{description}"',
            "",
            f"It matches {len(shortlist)} dependencies this repository declares. These mean different "
            "things and would give different answers, so choosing one for you would be guessing:",
            "",
        ]
        for index, candidate in enumerate(shown, start=1):
            lines.append(f"  {index}. {candidate}")
        if len(shortlist) > len(shown):
            lines.append(f"  ... and {len(shortlist) - len(shown)} more.")
        lines += [
            "",
            "Confirm which one is meant and call this tool again with that name in the description, "
            "or ask an engineer which of these implements the capability you have in mind.",
            "",
            "Refusing to pick is deliberate. The alternative is a confident, specific, wrong answer, "
            "which is the failure mode this tool exists to prevent.",
        ]
        return "\n".join(lines), False

    dependency = shortlist[0]
    inferred_weakly = not strong

    plan, scope_note, detail = _run_product_pivot(binary, repo, dependency, scope)
    if plan is None:
        return f"the analysis could not be completed, so no brief can be written:\n{detail}", True

    counts = plan.get("counts", {}) or {}
    items = plan.get("work_items", []) or []
    areas = product_areas(items)
    direct_areas = {name: data for name, data in areas.items() if data["direct"]}
    validate_areas = {name: data for name, data in areas.items() if data["validate"]}
    production_direct = sum(
        1 for item in items
        if item.get("verdict") == "INVALIDATED" and item.get("role") == "PRODUCTION"
    )
    klass, driver = decision_class(counts, direct_areas, production_direct)
    strength, coverage, interpretation = _confidence_block(plan, counts, scope_note)

    direct = counts.get("invalidated", 0)
    validate = counts.get("at_risk", 0)
    not_assessed = counts.get("unverified", 0) + counts.get("unreached", 0)
    excluded = counts.get("excluded_tests", 0)

    if direct == 0:
        headline = (
            f"No use of {dependency} was found in the code that was analyzed. "
            "That is not the same as it being unused -- see coverage below."
        )
    elif len(direct_areas) == 1:
        headline = (
            f"{direct} place(s) use {dependency} directly, all inside one area "
            f"({next(iter(direct_areas))}). {validate} further place(s) would need re-checking."
        )
    else:
        headline = (
            f"{direct} place(s) across {len(direct_areas)} areas use {dependency} directly, "
            f"with {validate} further place(s) needing re-checking afterwards."
        )

    lines = [
        "DECISION BRIEF",
        f"  {headline}",
        "",
        "WHAT WE UNDERSTOOD",
        f'  Your description: "{description}"',
        f"  Technical target used: {dependency}",
    ]
    if inferred_weakly:
        lines.append(
            "  This match is partial -- your wording appears inside the dependency name rather than "
            "matching it outright. Confirm it before this brief is used in a decision."
        )
    else:
        lines.append(
            "  This was matched against the dependencies this repository actually declares, not "
            "invented. Confirm it is the one you meant before this brief is used in a decision."
        )
    lines += [
        "",
        f"SCOPE CLASS: {klass}",
        f"  Driven by: {driver}.",
        "  This is a shape, not a schedule. No estimate of effort or duration is offered, because "
        "structural analysis cannot see the things that actually determine either.",
        "",
    ]

    if direct_areas:
        lines.append("PRODUCT AREAS DIRECTLY AFFECTED")
        for name, data in sorted(direct_areas.items(), key=lambda kv: -kv[1]["direct"]):
            lines.append(
                f"  {name:<40} {data['direct']} directly affected, {data['validate']} needing validation"
            )
        lines.append(
            "  These groupings are directories, not user-facing capabilities. A real capability map "
            "needs ownership metadata this repository does not carry, so it is not offered."
        )
        lines.append("")
    if validate_areas and not direct_areas:
        lines.append("AREAS NEEDING VALIDATION")
        for name, data in sorted(validate_areas.items(), key=lambda kv: -kv[1]["validate"])[:8]:
            lines.append(f"  {name:<40} {data['validate']} needing validation")
        lines.append("")

    lines += [
        "THE WORK THAT MUST HAPPEN",
        f"  {direct} place(s) use it directly and cannot stay as they are"
        + (f", of which {production_direct} are in shipped production code." if direct else "."),
        "",
        "VALIDATION SCOPE",
        f"  {validate} further place(s) depend on those and need re-checking once the direct uses "
        "are replaced. They break no rule themselves.",
    ]
    if excluded:
        lines.append(
            f"  {excluded} test file(s) were excluded before analysis and carry no finding either way."
        )
    lines += [
        "",
        "WHAT COULD NOT BE ASSESSED",
    ]
    if not_assessed:
        lines.append(
            f"  {not_assessed} file(s) were not assessed. Nothing was found in them because nothing "
            "could be looked for -- treat them as unexamined, not as clear."
        )
    else:
        lines.append("  Every file in scope was readable by the analyzer.")
    lines.append(
        "  Connections made at run time -- plugin interfaces, reflection, machine-generated code -- "
        "leave no trace for any static analysis. This applies even where coverage was complete."
    )
    lines += [
        "",
        "EVIDENCE AND CONFIDENCE",
        f"  Evidence strength     {strength}",
        f"  Analysis coverage     {coverage}",
        f"  Product interpretation {interpretation}",
        "",
        "THE DECISION IN FRONT OF YOU",
        f"  {_next_decision(decision_needed, klass, counts)}",
        "",
        "OPEN QUESTIONS SOMEONE MUST ANSWER",
        "  - Does the constraint cover test and internal tooling use, or only shipped production code?",
        f"  - Is {dependency} the right technical target for what you were told?",
    ]
    if not_assessed:
        lines.append("  - Who can confirm what is in the files that could not be assessed?")
    lines.append(
        "  - Which team owns the areas listed above? The mapping from directory to capability is "
        "the weakest link in this brief."
    )
    lines += [
        "",
        "This tool scopes decisions. It does not change code and cannot be asked to.",
        "",
        "--- structured_evidence (for follow-up questions; not for reading aloud) ---",
    ]

    structured = {
        "vocabulary_note": (
            "These labels are deliberately different from the engineering tool's own. They are "
            "chosen so a non-specialist cannot read a coverage gap as a clean result. Use them "
            "verbatim when answering follow-ups; do not substitute the engineering terms."
        ),
        "understood": {
            "change_description": description,
            "technical_target": dependency,
            "match_quality": "segment match" if strong else "partial match",
            "requires_confirmation": True,
            "candidates_considered": len(candidates),
        },
        "decision_class": {"class": klass, "driver": driver, "effort_estimate": None},
        "counts": {
            PRODUCT_LABELS["INVALIDATED"]: direct,
            PRODUCT_LABELS["AT-RISK"]: validate,
            PRODUCT_LABELS["SAFE"]: counts.get("safe", 0),
            PRODUCT_LABELS["UNREACHED"]: not_assessed,
            "excluded_from_analysis": excluded,
            "total_analyzed": counts.get("total", 0),
        },
        "areas": {
            name: {
                "directly_affected": data["direct"],
                "needs_validation": data["validate"],
                "examples": data["paths"],
            }
            for name, data in sorted(direct_areas.items(), key=lambda kv: -kv[1]["direct"])
        },
        "areas_are": "directories, not a capability map; ownership metadata is absent",
        "confidence": {
            "evidence_strength": strength,
            "analysis_coverage": coverage,
            "product_interpretation": interpretation,
        },
        "next_step": _next_decision(decision_needed, klass, counts),
        "can_modify_code": False,
    }
    lines.append(json.dumps(structured, indent=2))
    return "\n".join(lines), False


def tool_explain_product_impact(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    dependency = args.get("dependency")
    area = args.get("area")
    if not repo or not dependency or not area:
        return (
            "explain_product_impact requires `repo`, `dependency` (the confirmed technical target, "
            "as returned by assess_product_constraint_change) and `area` (a directory or module "
            "prefix from that brief).",
            True,
        )

    plan, scope_note, detail = _run_product_pivot(binary, repo, dependency, args.get("scope", "unspecified"))
    if plan is None:
        return f"the analysis could not be completed:\n{detail}", True

    items = [
        item for item in (plan.get("work_items", []) or [])
        if (item.get("path") or "").startswith(area)
    ]
    if not items:
        return (
            f"Nothing under `{area}` appears in this analysis at all. Either the prefix is wrong, or "
            f"that area was outside the scope that ran. It is NOT a statement that {area} is clear.",
            False,
        )

    direct = [i for i in items if i.get("verdict") == "INVALIDATED"]
    validate = [i for i in items if i.get("verdict") == "AT-RISK"]
    unassessed = [i for i in items if i.get("verdict") in ("UNVERIFIED", "UNREACHED")]
    clear = [i for i in items if i.get("verdict") == "SAFE"]
    production = [i for i in direct + validate if i.get("role") == "PRODUCTION"]
    support = [i for i in direct + validate if i.get("role") != "PRODUCTION"]

    confirmed = sum(1 for i in direct + validate if i.get("evidence_quality") == "CONFIRMED")
    inferred = sum(1 for i in direct + validate if i.get("evidence_quality") == "HEURISTIC")
    unreadable = sum(1 for i in direct + validate if i.get("evidence_quality") == "UNVERIFIED")

    lines = [
        f"WHY {area} IS IN THIS ASSESSMENT",
        "",
        f"  {len(items)} file(s) under {area} were looked at for {dependency}.",
        "",
        "WHAT THE CONNECTION IS",
    ]
    if direct:
        lines.append(
            f"  {len(direct)} file(s) use {dependency} directly. This area is included because of "
            "its own code, not because of something it depends on."
        )
        for item in direct[:6]:
            lines.append(f"    - {item.get('path')}")
        if len(direct) > 6:
            lines.append(f"    ... and {len(direct) - 6} more.")
    if validate:
        lines.append(
            f"  {len(validate)} file(s) do not use it themselves but depend on something that does. "
            "They need re-checking after the direct uses are replaced, not rewriting."
        )
    if not direct and not validate:
        lines.append(
            "  No connection was found in this area. Given the coverage limits below, read that as "
            "'nothing found here', not as 'this area is unaffected'."
        )

    lines += [
        "",
        "SHIPPED CODE VERSUS SUPPORTING CODE",
        f"  {len(production)} of the affected file(s) are production code that ships.",
        f"  {len(support)} are tests, fixtures, generated or vendored files.",
        "  This distinction usually matters more than the raw count: a large number driven by one "
        "shared test helper is a small job, not a large one.",
        "",
        "HOW WELL THIS IS KNOWN",
    ]
    if confirmed or inferred or unreadable:
        lines.append(
            f"  {confirmed} connection(s) were read directly from source. {inferred} were inferred "
            f"from a matching name and could be wrong. {unreadable} came from files that did not "
            "fully parse."
        )
        if inferred:
            lines.append(
                "  The inferred ones are worth an engineer's eye before this area is committed to a plan."
            )
    else:
        lines.append("  No graded connections in this area.")

    lines += [
        "",
        "WHAT WAS NOT ESTABLISHED",
        f"  {len(unassessed)} file(s) here could not be assessed.",
        f"  {len(clear)} file(s) had no connection found, which is bounded by the same coverage limits: "
        "run-time connections through interfaces, reflection or generated code leave no trace.",
    ]
    if scope_note:
        lines.append(f"  {scope_note}")
    lines.append("")
    lines.append(
        "This explains an area. It does not estimate what changing it would take, and nothing here "
        "should be converted into a date."
    )
    return "\n".join(lines), False


def tool_prepare_engineering_discovery_brief(args):
    binary, error = resolve_binary()
    if error:
        return error, True

    repo = args.get("repo")
    dependency = args.get("dependency")
    if not repo or not dependency:
        return (
            "prepare_engineering_discovery_brief requires `repo` and `dependency` (the CONFIRMED "
            "technical target). Confirm it with assess_product_constraint_change first: this brief "
            "is a handoff, and handing engineering an unconfirmed target wastes the discovery.",
            True,
        )

    scope = args.get("scope", "unspecified")
    plan, scope_note, detail = _run_product_pivot(binary, repo, dependency, scope)
    if plan is None:
        return f"the analysis could not be completed, so no brief can be written:\n{detail}", True

    counts = plan.get("counts", {}) or {}
    items = plan.get("work_items", []) or []
    areas = product_areas(items)
    direct_areas = {name: data for name, data in areas.items() if data["direct"]}
    production_roots = [
        item for item in items
        if item.get("verdict") == "INVALIDATED" and item.get("role") == "PRODUCTION"
    ]
    other_roots = [
        item for item in items
        if item.get("verdict") == "INVALIDATED" and item.get("role") != "PRODUCTION"
    ]
    needs_confirming = [
        item for item in items
        if item.get("verification_required") and item.get("verdict") in ("INVALIDATED", "AT-RISK")
    ]
    not_assessed = counts.get("unverified", 0) + counts.get("unreached", 0)
    exclude_tests, _ = _scope_flags(scope)

    command = (
        f"entire graph pivot --repo . --dependency {dependency} --depth 2"
        + (" --exclude-tests" if exclude_tests else "")
        + " --format plan"
    )

    lines = [
        f"ENGINEERING DISCOVERY BRIEF: {dependency}",
        "",
        "CONFIRMED TARGET",
        f"  {dependency}",
        "  Confirmed by the requester. If this is wrong, stop -- everything below is wrong with it.",
        "",
        "WHAT HAS TO CHANGE",
        f"  {len(production_roots)} place(s) in shipped production code use it directly.",
    ]
    for item in production_roots[:10]:
        lines.append(f"    - {item.get('path')}")
    if len(production_roots) > 10:
        lines.append(f"    ... and {len(production_roots) - 10} more.")
    if other_roots:
        lines.append(
            f"  {len(other_roots)} further direct use(s) are in tests, fixtures, generated or "
            "vendored files. Whether those are in scope is a policy question, not a technical one."
        )

    lines += [
        "",
        "VALIDATION SCOPE",
        f"  {counts.get('at_risk', 0)} place(s) depend on the above and need re-checking after the "
        "direct uses are replaced.",
        f"  Spread across {len(direct_areas)} area(s): " + (", ".join(sorted(direct_areas)) or "none"),
        "",
        "WHAT NOBODY HAS LOOKED AT",
        f"  {not_assessed} file(s) were not assessed -- unreadable, or written in a language the "
        "analyzer indexes without following connections.",
    ]
    if needs_confirming:
        lines.append(
            f"  {len(needs_confirming)} finding(s) rest on inferred connections rather than ones read "
            "directly from source. Open the cited line before relying on any of them."
        )
    lines += [
        "  Run-time connections through interfaces, reflection or generated code leave no trace for "
        "static analysis, so a clean result is never proof of absence.",
        "",
        "OPEN QUESTIONS FOR PRODUCT, LEGAL OR DESIGN",
        "  - Does the constraint cover test and internal tooling use, or only shipped production code?",
        "  - Is a like-for-like replacement acceptable, or must the capability be removed entirely?",
        "  - Is there a deadline that changes the approach (staged migration versus one change)?",
        "  - Who owns each affected area, and are they aware this is coming?",
        "",
        "THE TECHNICAL WORK ORDER",
        "  Engineering can reproduce this analysis and get the machine-readable work order with:",
        f"    {command}",
        "  That output carries one work item per file, its priority, the evidence behind it, and a "
        "flag on every item whose evidence needs confirming against source.",
        "",
        "SCOPE OF THIS BRIEF",
        "  This is a discovery brief, not an implementation instruction and not a schedule. It "
        "deliberately contains no estimate. Nothing here authorises changing code.",
    ]
    if scope_note:
        lines += ["", f"  Note on scope: {scope_note}"]
    return "\n".join(lines), False


TOOLS = [
    {
        "name": "pivot",
        "description": (
            "A requirement changed after the code was written: some dependency is now prohibited. "
            "Classify every file in a repository as INVALIDATED (it reaches the dependency), AT-RISK "
            "(it depends on something invalidated), SAFE (parsed cleanly, no path found), UNVERIFIED "
            "(the parser could not read it, so no path found proves nothing) or UNREACHED "
            "(inventory-only language, never analysed for relations). Every verdict cites the graph "
            "edge that produced it.\n\n"
            "HOW TO USE THE RESULT: with format=plan, every work item carries `evidence_quality` "
            "(CONFIRMED | HEURISTIC | UNVERIFIED) and `verification_required`. CONFIRMED means the "
            "parser resolved the edge structurally -- an import declaration it read, or a call it "
            "followed to a definition. HEURISTIC means it was inferred from a name, a package or an "
            "inferred type: often right, never proof. UNVERIFIED means the file itself did not parse. "
            "You MUST open the cited source line, or run a test, before acting on or reporting done "
            "any item whose evidence_quality is not CONFIRMED. Do not treat a heuristic edge as an "
            "established fact, and do not count an UNVERIFIED file towards the constraint being "
            "satisfied. Read `known_blind_spots` and `execution_contract` before the work items: they "
            "are placed first because how far to trust a plan is decided before it is read.\n\n"
            "This takes a package name, not a sentence. Turning a requirement into a package name is "
            "the caller's job and stays outside this tool deliberately."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "repo": {"type": "string", "description": "Path to the repository to analyse."},
                "dependency": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Package name(s) now prohibited, e.g. ['os/exec']. Not prose.",
                },
                "depth": {
                    "type": "integer",
                    "default": 2,
                    "description": "How many hops of propagation to walk. Capped low on purpose: unbounded closure marks the whole repository at-risk, which is true and useless.",
                },
                "exclude_tests": {
                    "type": "boolean",
                    "default": False,
                    "description": "Drop test files BEFORE classification, so a production file reached only through a test comes back safe. The count of excluded files is reported: a file never classified is not a file that came back safe.",
                },
                "format": {
                    "type": "string",
                    "enum": ["text", "json", "plan"],
                    "default": "plan",
                    "description": "plan is the agent-facing work order (recommended); text is the human report; json is the full classification.",
                },
            },
            "required": ["repo", "dependency"],
        },
    },
    {
        "name": "pivot_verify",
        "description": (
            "Grade the commit an agent produced against a plan captured before it started. "
            "Deterministic: no model and no judgement, only files on disk, `git diff --numstat`, and "
            "the counts from re-running the analysis. It exists because `invalidated == 0` is "
            "satisfied by DELETING the code as well as by fixing it, so the count alone was never "
            "evidence.\n\n"
            "Returns one of four verdicts, and the difference between them matters:\n"
            "  PASS          every check ran, every check held, nothing left unchecked. Rare by design.\n"
            "  FAIL          a check ran and did not hold.\n"
            "  PARTIAL       THE NORMAL GOOD OUTCOME. Everything that ran held, but coverage is "
            "incomplete: whether behaviour was preserved and whether a heuristic edge holds cannot be "
            "decided from a diff. Treat it as success with a named remainder, not as a near-miss.\n"
            "  CANNOT_VERIFY a check could not run at all. This is an absence of evidence and must "
            "NEVER be reported as a pass.\n\n"
            "If after_plan is omitted the analysis is re-run automatically using the before-plan's own "
            "flags, because a re-run asking a different question is not comparable."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "repo": {"type": "string", "description": "Path to the repository holding the change."},
                "before_plan": {
                    "type": "string",
                    "description": "Path to the plan JSON captured BEFORE the agent started. This is the authoritative baseline; a baseline produced after the fact proves nothing.",
                },
                "range": {
                    "type": "string",
                    "description": "Git range holding the agent's change, e.g. HEAD~1..HEAD.",
                },
                "after_plan": {
                    "type": "string",
                    "description": "Optional path to a plan from re-running pivot on the result. Omit it and the re-run happens here with the before-plan's flags.",
                },
            },
            "required": ["repo", "before_plan", "range"],
        },
    },
    {
        "name": "impact",
        "description": (
            "Blast radius for one symbol before you change it: direct and transitive callers, callees, "
            "type consumers, data flows, files that historically change with it, and same-container "
            "siblings. Answers 'what breaks if I change X' in one call.\n\n"
            "The result includes the parser's own Completeness line, which bounds everything else in "
            "it. An absent relation is not proof of no relation: interface dispatch, reflection and "
            "generated code leave no edge, so 0 callers can mean none were found rather than none "
            "exist."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "repo": {"type": "string", "description": "Path to the repository."},
                "symbol": {"type": "string", "description": "Symbol name to analyse."},
                "file": {
                    "type": "string",
                    "description": "Required when the symbol name is ambiguous; the graph returns the definition list until you disambiguate.",
                },
                "depth": {"type": "integer", "enum": [1, 2], "default": 2, "description": "Transitive caller depth."},
            },
            "required": ["repo", "symbol"],
        },
    },
    {
        "name": "capabilities",
        "description": (
            "Which languages this graph parses semantically and which are inventory-only. Check it "
            "before trusting any relation-based answer. Inventory-only files get file records but no "
            "call or import relations, so no relation is ever found in them -- which is why pivot "
            "reports them UNREACHED rather than SAFE. A SAFE verdict on an inventory-only file would "
            "assert an absence nobody checked."
        ),
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "assess_product_constraint_change",
        "description": (
            "FOR PRODUCT, DESIGN, LEGAL OR COMPLIANCE, NOT FOR ENGINEERING. Assess the likely product "
            "impact of a business, legal, compliance, vendor or requirement change. Takes the "
            "constraint in the words the person received it in -- \"Legal says we can no longer use "
            "Vendor X for customer data\" -- NOT a package name; working out which dependency is "
            "meant is part of what this does. If the description matches more than one dependency it "
            "returns the candidates and refuses to proceed, because a confident answer about the "
            "wrong dependency is worse than no answer. Returns a decision brief: what was understood, "
            "which product areas are affected, what must change, what could not be assessed, and the "
            "decision now in front of the reader. It NEVER estimates effort or duration -- structural "
            "analysis cannot see what determines either -- and it never reports a clean result "
            "without the coverage caveat that bounds it. It cannot change code."
        ),
        "inputSchema": {
            "type": "object",
            "required": ["repo", "change_description"],
            "properties": {
                "repo": {"type": "string", "description": "Path to the repository to analyse."},
                "change_description": {
                    "type": "string",
                    "description": (
                        "The constraint in plain language, as the person who received it would say "
                        "it. Do not translate it into a package name first."
                    ),
                },
                "scope": {
                    "type": "string",
                    "enum": ["production_only", "all_code", "unspecified"],
                    "default": "unspecified",
                    "description": (
                        "Whether the constraint covers only shipped production code. Leaving it "
                        "unspecified analyses everything and says so, because test-only use is a "
                        "policy question the tool must not decide."
                    ),
                },
                "decision_needed": {
                    "type": "string",
                    "enum": ["feasibility", "scope", "risk_review", "prioritization", "engineering_discovery"],
                    "default": "scope",
                    "description": "What the reader has to decide; shapes the recommendation.",
                },
            },
        },
    },
    {
        "name": "explain_product_impact",
        "description": (
            "FOR PRODUCT OR DESIGN. Explain why one area appears in a constraint assessment: whether "
            "it uses the dependency itself or merely depends on something that does, how much is "
            "shipped code versus tests and fixtures, how directly the connection was established, "
            "and what was not established. Use it when an affected-file count looks alarming and "
            "nobody can tell whether it means anything -- a large number driven by one shared test "
            "helper is a small job. Requires a CONFIRMED dependency name from "
            "assess_product_constraint_change. Estimates nothing."
        ),
        "inputSchema": {
            "type": "object",
            "required": ["repo", "dependency", "area"],
            "properties": {
                "repo": {"type": "string"},
                "dependency": {
                    "type": "string",
                    "description": "The confirmed technical target, not a business name.",
                },
                "area": {
                    "type": "string",
                    "description": "A directory or module prefix, as listed in the decision brief.",
                },
                "scope": {
                    "type": "string",
                    "enum": ["production_only", "all_code", "unspecified"],
                    "default": "unspecified",
                },
            },
        },
    },
    {
        "name": "prepare_engineering_discovery_brief",
        "description": (
            "THE HANDOFF. Turn a confirmed constraint assessment into a bounded brief engineering can "
            "act on: the confirmed target, what must change in shipped code, what needs re-checking "
            "afterwards, what nobody has looked at, the open questions product/legal/design still owe "
            "an answer to, and the exact command that produces the machine-readable work order for a "
            "coding agent. Requires a CONFIRMED dependency name. This turns \"can we do this?\" into a "
            "scoped discovery task without pretending to forecast delivery. It authorises nothing and "
            "changes nothing."
        ),
        "inputSchema": {
            "type": "object",
            "required": ["repo", "dependency"],
            "properties": {
                "repo": {"type": "string"},
                "dependency": {
                    "type": "string",
                    "description": "The confirmed technical target, not a business name.",
                },
                "scope": {
                    "type": "string",
                    "enum": ["production_only", "all_code", "unspecified"],
                    "default": "unspecified",
                },
            },
        },
    },
]

HANDLERS = {
    "pivot": tool_pivot,
    "pivot_verify": tool_pivot_verify,
    "impact": tool_impact,
    "capabilities": tool_capabilities,
    "assess_product_constraint_change": tool_assess_product_constraint_change,
    "explain_product_impact": tool_explain_product_impact,
    "prepare_engineering_discovery_brief": tool_prepare_engineering_discovery_brief,
}


# --------------------------------------------------------------------------
# JSON-RPC 2.0 over stdio
# --------------------------------------------------------------------------

PARSE_ERROR, INVALID_REQUEST, METHOD_NOT_FOUND, INVALID_PARAMS = -32700, -32600, -32601, -32602


def respond(request_id, result):
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def error(request_id, code, message):
    return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}}


def _initialize(params):
    # Echo the client's protocol version when it names one. We implement a
    # version-agnostic subset, so insisting on our own preference would only
    # cause a host that speaks a different revision to hang up on a server it
    # could have talked to.
    version = params.get("protocolVersion")
    if not isinstance(version, str) or not version:
        version = DEFAULT_PROTOCOL_VERSION
    return {
        "protocolVersion": version,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
        "instructions": (
            "PivotMap over MCP: analysis and verification for a requirement that changed after the "
            "code was written. The graph is evidence, not an oracle -- every answer states how well "
            "it is known. Anything not marked CONFIRMED must be checked against source before you act "
            "on it or report it done, and a verdict of CANNOT_VERIFY is never a pass."
        ),
    }


def handle(message):
    """Handle one message. Returns a response dict, or None for a notification."""
    if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
        return error(None, INVALID_REQUEST, "expected a JSON-RPC 2.0 object")

    method = message.get("method")
    request_id = message.get("id")
    params = message.get("params") or {}

    # A notification has no id and must not be answered, even when it is one we
    # do not implement. Replying would put an unexpected frame on the stream.
    if request_id is None:
        if method not in ("notifications/initialized", "notifications/cancelled"):
            log(f"ignoring unknown notification {method!r}")
        return None

    if method == "initialize":
        return respond(request_id, _initialize(params))

    if method == "ping":
        return respond(request_id, {})

    if method == "tools/list":
        return respond(request_id, {"tools": TOOLS})

    if method == "tools/call":
        name = params.get("name")
        handler = HANDLERS.get(name)
        if handler is None:
            return error(
                request_id,
                INVALID_PARAMS,
                f"unknown tool {name!r}. Available: {', '.join(sorted(HANDLERS))}",
            )
        arguments = params.get("arguments") or {}
        if not isinstance(arguments, dict):
            return error(request_id, INVALID_PARAMS, "`arguments` must be an object")
        try:
            text, is_error = handler(arguments)
        except Exception as exc:  # a crashed tool must not take the server down
            log(f"tool {name} raised {type(exc).__name__}: {exc}")
            text, is_error = f"{name} failed unexpectedly: {type(exc).__name__}: {exc}", True
        return respond(request_id, {"content": [{"type": "text", "text": text}], "isError": is_error})

    return error(request_id, METHOD_NOT_FOUND, f"method not found: {method!r}")


def main():
    binary, problem = resolve_binary()
    log(f"{SERVER_NAME} {SERVER_VERSION} ready; binary: {binary or 'MISSING'}")
    if problem:
        # Not fatal. The host expects a server on stdio, and a tool call that
        # explains the missing binary is far more useful than a process that
        # exits before initialize and leaves the host with no error at all.
        log(problem)

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except json.JSONDecodeError as exc:
            print(json.dumps(error(None, PARSE_ERROR, f"invalid JSON: {exc}")), flush=True)
            continue
        response = handle(message)
        if response is not None:
            print(json.dumps(response), flush=True)


if __name__ == "__main__":
    main()
