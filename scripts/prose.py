#!/usr/bin/env python3
"""Hold the enforceable subset of nanohype/standards/documentation-voice.json.

That standard says of itself: "largely unenforceable by pattern matching, and a gate
claiming coverage it does not have is itself the failure mode the stack is built to
avoid." It then names the subset a gate CAN hold — verification tallies, prose addressed to
an agent, bare TODO or FIXME with no owner, and internal issue or PR references outside a
maintained exclusion list.

This holds exactly that subset and claims nothing more. The rest — narration against an
unstated past, rationale braided with process, self-defence — is review, because the same
violation arrives in as many wordings as there are authors and a pattern finds only the
ones its author already thought of.

Scope is every prose surface, not only .md: Go comments, markdown, YAML comments, and the
help text and error strings a human reads. Go string literals are scanned too, because a
CLI's help text and error messages are prose that ships.

The positive controls run on EVERY invocation. Each introduces the exact violation a rule
exists to catch, and the clean fixture is asserted to pass first — without that a rejection
proves nothing, since the gate might have been failing for an unrelated reason all along.
"""

import os
import re
import sys

REPO = os.environ.get("REPO_ROOT", ".")

SKIP_DIRS = {".git", "vendor", "node_modules", "dist", "bin"}
PROSE_SUFFIXES = (".go", ".md", ".yml", ".yaml", ".sh", ".py")

# ── the rules ────────────────────────────────────────────────────────────────
#
# Each is a shape the standard names as gate-holdable, stated so a failure says what to
# write instead rather than only what is wrong.
RULES = [
    (
        "internal-reference",
        re.compile(r"""(?xi)
            \bledger\s+O\d+\b            # "ledger O27" — an internal tracker
          | \(\s*O\d{1,2}\s*\)           # "(O24)"
          | \b[a-z][\w-]*\#\d+\b         # "landing-zone#205" — a cross-repo PR
          | (?<![\w/])\#\d{1,5}\b        # "(#16)" — a bare issue number
          | \btarget\s+\d+\b             # "once target 11 lands" — a roadmap item
        """),
        "an internal issue, PR or ledger id the reader cannot resolve. State the behaviour "
        "the reference described — that makes the prose better rather than shorter",
    ),
    (
        "verification-tally",
        re.compile(r"""(?xi)
            \bobserved(?:\s+on\b|:)                       # "Observed: ...", "Observed on a live cluster"
          | \bdiscovered\s+\d+\s+\w+\s+in\b               # "Discovered 6 minutes in"
          | \b(?:this\s+)?session\s+alone\b
          | \b(?:shipped|found|turned\s+up|left(?:\s+\w+)?)\s+
              (?:two|three|four|five|six|seven|eight|nine|ten|\d+)\s+such\b
          | \b(?:left|leaving)\s+(?:two|three|four|five|six|seven|eight|nine|ten|\d+)\s+
              (?:unattached|orphaned|stranded)\b
          | \b\d+\s*/\s*\d+-(?:healthy|green|passing)\b   # "a 44/44-healthy cluster"
          | \b(?:all|only)\s+\d+\s+(?:Applications|findings|hits|files|symbols)\b
          | \bstood\s+at\s+\d+\b
        """),
        "a measurement stated as documentation. It was true when written and nothing keeps "
        "it true. State the requirement it was evidence for, or the invariant a gate now "
        "enforces, or move the figure to log.md",
    ),
    (
        "agent-addressed",
        re.compile(r"""(?xi)
            \bfuture\s+claude\b | \bfor\s+the\s+next\s+agent\b
          | \bas\s+discussed\b | \bwe\s+decided\b | \bturns\s+out\b
          | \bafter\s+some\s+digging\b | \bin\s+this\s+pass\b
          | \bkept\s+here\s+for\b
        """),
        "session narration. The reader was not in the room",
    ),
    (
        "line-number-citation",
        re.compile(r"\b[\w./-]+\.(?:go|tf|hcl|ya?ml|sh|py)\s*:\s*\d+(?:\s*-\s*\d+)?\b"),
        "a citation by line number. It is stale at the next edit above it, in either this "
        "repo or the one it names, and nothing signals when it drifts — name the symbol, "
        "the file, or the behaviour instead",
    ),
    (
        "unowned-todo",
        re.compile(r"(?i)\b(?:TODO|FIXME|XXX|HACK)\b(?!\s*\([^)]+\))"),
        "a marker with no owner. Either do it, or state the constraint that makes the "
        "current shape correct",
    ),
]

# Lines this gate must not read as prose. Each is ASSERTED: an entry that stops matching
# anything fails the run, so an exemption cannot outlive what it exempted.
EXEMPT = [
    (
        re.compile(r"scripts/prose\.py$"),
        "this file, whose rules and controls necessarily quote the shapes they reject",
    ),
]


def is_exempt(path):
    return any(r.search(path) for r, _ in EXEMPT)


# ── prose extraction ─────────────────────────────────────────────────────────
GO_LINE_COMMENT = re.compile(r"//(.*)$")
HASH_COMMENT = re.compile(r"(?:^|\s)#(.*)$")
GO_STRING = re.compile(r'"((?:[^"\\]|\\.)*)"|`([^`]*)`')


def prose_lines(path, text):
    """Yield (lineno, prose) for every human-readable span in a file.

    Extracting rather than stripping: a gate that reads code as prose produces noise, and
    one that reads only code misses the comment standing where an implementation should be.
    """
    suffix = os.path.splitext(path)[1]
    for n, raw in enumerate(text.splitlines(), 1):
        if suffix == ".go":
            m = GO_LINE_COMMENT.search(raw)
            if m:
                yield n, m.group(1)
            for sm in GO_STRING.finditer(raw):
                yield n, sm.group(1) or sm.group(2) or ""
        elif suffix in (".yml", ".yaml", ".sh", ".py"):
            m = HASH_COMMENT.search(raw)
            if m:
                yield n, m.group(1)
        else:
            yield n, raw


def walk(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in sorted(filenames):
            if not name.endswith(PROSE_SUFFIXES):
                continue
            full = os.path.join(dirpath, name)
            rel = os.path.relpath(full, root)
            if is_exempt(rel):
                continue
            try:
                yield rel, open(full, encoding="utf-8").read()
            except (UnicodeDecodeError, OSError):
                continue


def check(root, assert_exemptions=False):
    findings, scanned = [], 0
    exempt_hits = {r.pattern: 0 for r, _ in EXEMPT}

    for rel, text in walk(root):
        scanned += 1
        for n, prose in prose_lines(rel, text):
            for rule, pattern, remedy in RULES:
                m = pattern.search(prose)
                if m:
                    findings.append((rel, n, rule, m.group(0).strip(), remedy))

    if scanned == 0:
        findings.append(("", 0, "empty-enumeration", "",
                         "no prose files were scanned at all — a walk that matches nothing "
                         "is how a gate reports success over zero files"))

    if assert_exemptions:
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for name in filenames:
                rel = os.path.relpath(os.path.join(dirpath, name), root)
                for r, _ in EXEMPT:
                    if r.search(rel):
                        exempt_hits[r.pattern] += 1
        for r, why in EXEMPT:
            if exempt_hits[r.pattern] == 0:
                findings.append(("", 0, "dead-exemption", r.pattern,
                                 f"this exemption ({why}) matches nothing — an exemption that "
                                 "outlives what it exempted hides the next violation of that shape"))

    return scanned, findings


# ── positive controls ────────────────────────────────────────────────────────
CLEAN = """\
// Package x does a thing.
//
// The constraint: a value must fail rather than silently resolve, because a fallback here
// would be reachable exactly when the guarantee is absent.
package x
"""

CONTROLS = [
    ("a ledger id", CLEAN + "\n// Settled upstream. Ledger O27.\n"),
    ("a bracketed ledger id", CLEAN + "\n// The Neuron half went first (O24).\n"),
    ("a cross-repo PR reference", CLEAN + "\n// Added by landing-zone#205.\n"),
    ("a bare issue number", CLEAN + "\n// Re-runnable by design (#16).\n"),
    ("a roadmap item", CLEAN + "\n// Available once target 11 lands.\n"),
    ("an observation tally", CLEAN + "\n// Observed: a fresh install generated 44 Applications.\n"),
    ("a time-to-discovery tally", CLEAN + "\n// Unrecoverable by retry. Discovered 6 minutes in.\n"),
    ("a session tally", CLEAN + "\n// This session alone turned up two such symbols.\n"),
    ("a leftover-resource tally", CLEAN + "\n// A failed install left three unattached volumes behind.\n"),
    ("a health-ratio tally", CLEAN + "\n// The only reason a 44/44-healthy cluster survived.\n"),
    ("session narration", CLEAN + "\n// As discussed, this is kept here for future Claude.\n"),
    ("an unowned TODO", CLEAN + "\n// TODO: wire this up.\n"),
    ("an in-repo line-number citation", CLEAN + "\n// The guard lives at phases/agentplatform.go:224.\n"),
    ("a cross-repo line-number citation", CLEAN + "\n// Matches operators/platform_iam.go:177-190.\n"),
    ("a violation inside a shipped error string", CLEAN +
     '\nvar e = "could not apply — see ledger O14"\n'),
]


def run_controls():
    if not CONTROLS:
        print("prose: this gate ships no positive controls", file=sys.stderr)
        sys.exit(1)

    import tempfile
    root = tempfile.mkdtemp()

    def lay(body):
        open(os.path.join(root, "x.go"), "w").write(body)

    lay(CLEAN)
    scanned, findings = check(root)
    if findings:
        print("prose: the clean control fixture does not pass, so no rejection below proves "
              "anything:", file=sys.stderr)
        for f in findings:
            print(f"  {f}", file=sys.stderr)
        sys.exit(1)
    print("control: clean prose passes")

    failed = False
    for name, body in CONTROLS:
        lay(body)
        _, findings = check(root)
        if findings:
            print(f"control: {name} rejected")
        else:
            print(f"control: {name} was ACCEPTED — this gate cannot catch it", file=sys.stderr)
            failed = True

    # The enumeration must fail on empty, not pass.
    empty = tempfile.mkdtemp()
    _, findings = check(empty)
    if findings:
        print("control: an empty enumeration rejected")
    else:
        print("control: an empty enumeration was ACCEPTED", file=sys.stderr)
        failed = True

    if failed:
        sys.exit(1)


def main():
    run_controls()
    if "--controls-only" in sys.argv:
        return

    scanned, findings = check(REPO, assert_exemptions=True)
    if findings:
        print(f"prose: {len(findings)} violation(s) of the enforceable subset of "
              f"documentation-voice:", file=sys.stderr)
        for path, n, rule, hit, remedy in findings:
            where = f"{path}:{n}" if path else "(tree)"
            print(f"  {where}  [{rule}]  {hit!r}", file=sys.stderr)
            print(f"      {remedy}", file=sys.stderr)
        sys.exit(1)

    print(f"prose: {scanned} file(s) scanned, no tallies, internal references, "
          f"agent-addressed prose or unowned markers")


if __name__ == "__main__":
    main()
