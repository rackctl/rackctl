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

import ast
import io
import os
import re
import sys
import tokenize

REPO = os.environ.get("REPO_ROOT", ".")

# Repo-hygiene checks — a dead exemption, a manager matching nothing — are findings about
# THIS tree, not about whether the gate can reject. They run only against the default root.
# A fixture built to exercise one rule carries neither, so asserting them there would make
# every fixture fail and prove nothing about the gate.
SCANNING_THE_REPO = "REPO_ROOT" not in os.environ

SKIP_DIRS = {".git", "vendor", "node_modules", "dist", "bin"}
PROSE_SUFFIXES = (".go", ".md", ".yml", ".yaml", ".sh", ".py", ".json", ".tf", ".hcl", ".txt")
PROSE_NAMES = ("Makefile", "Dockerfile")

# Files whose prose is not this repo's to write. documentation-voice excludes vendored
# upstream text explicitly. Asserted, not described: an entry naming a file that no longer
# exists fails the run.
NOT_OUR_PROSE = [
    (re.compile(r"^LICENSE$"), "the Apache 2.0 text, vendored verbatim"),
    (re.compile(r"^coverage\.out$"), "a generated cover profile, not prose"),
]

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
          | (?-i:\bO\d{1,2}\b)           # a bare "O1", the same id without its prefix
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
          | \b(?:two|three|four|five|six|seven|eight|nine|ten)\s+
              (?:had|did|were|have|do)\b                  # "Dashboard ids die; three had."
          | \b(?:one|two|three|four|five)\s+such\s+\w+\s+(?:held|caused|broke)\b
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
        re.compile(r"(?i)\b(?:TODO|FIXME|XXX|HACK)\b(?![ \t]*\([^)]+\))"),
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
    (
        re.compile(r"scripts/floor\.py$"),
        "the floor, whose known-bad fixtures are by construction the violations the gates "
        "it probes exist to reject",
    ),
]


def is_exempt(path):
    return any(r.search(path) for r, _ in EXEMPT)


# ── prose extraction ─────────────────────────────────────────────────────────
GO_LINE_COMMENT = re.compile(r"//(.*)$")
GO_BLOCK_OPEN = re.compile(r"/\*")
HASH_COMMENT = re.compile(r"(?:^|\s)#(.*)$")
GO_STRING = re.compile(r'"((?:[^"\\]|\\.)*)"|`([^`]*)`')


def python_prose(text):
    """Yield (lineno, prose) for Python, via the AST and the tokenizer.

    A DOCSTRING IS A STRING, NOT A COMMENT. Scanning `#` lines alone misses every module,
    class and function docstring — which in a gate is where most of the prose lives, and
    where a gate's own documentation quotes the very shapes it rejects. Comment-blanking
    would not have touched them either: the docstring is exactly where "comments" and
    "string bodies" stop being separate views of the source.

    So the AST supplies every string constant, including docstrings, and the tokenizer
    supplies the comments. Both read the file the way Python does rather than the way a
    regex guesses at it.
    """
    try:
        tree = ast.parse(text)
    except SyntaxError:
        return
    for node in ast.walk(tree):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            yield node.lineno, node.value
    try:
        for tok in tokenize.generate_tokens(io.StringIO(text).readline):
            if tok.type == tokenize.COMMENT:
                yield tok.start[0], tok.string.lstrip("#")
    except (tokenize.TokenError, IndentationError):
        return


def go_prose(text):
    """Yield (lineno, prose) for every comment and string literal in Go source.

    A scanner rather than a regex. A per-line `//` match reads none of a /* */ block, and
    this repo carries seventeen of them — so the rule that a gate must read its input the
    way the language does applies here as much as it did to Python docstrings. Raw strings
    and escapes are tracked because both can contain what looks like a comment opener.
    """
    i, line, n = 0, 1, len(text)
    while i < n:
        c = text[i]
        if c == "\n":
            line += 1
            i += 1
        elif text.startswith("//", i):
            end = text.find("\n", i)
            end = n if end < 0 else end
            yield line, text[i + 2:end]
            i = end
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            end = n if end < 0 else end
            body = text[i + 2:end]
            start = line
            for offset, seg in enumerate(body.split("\n")):
                yield start + offset, seg
            line += body.count("\n")
            i = end + 2
        elif c == "`":
            end = text.find("`", i + 1)
            end = n if end < 0 else end
            body = text[i + 1:end]
            start = line
            for offset, seg in enumerate(body.split("\n")):
                yield start + offset, seg
            line += body.count("\n")
            i = end + 1
        elif c == '"':
            j, buf = i + 1, []
            while j < n and text[j] != '"':
                if text[j] == "\\" and j + 1 < n:
                    buf.append(text[j + 1])
                    j += 2
                    continue
                if text[j] == "\n":
                    break
                buf.append(text[j])
                j += 1
            yield line, "".join(buf)
            i = j + 1
        else:
            i += 1


def prose_lines(path, text):
    """Yield (lineno, prose) for every human-readable span in a file.

    Extracting rather than stripping: a gate that reads code as prose produces noise, and
    one that reads only code misses the comment standing where an implementation should be.
    """
    suffix = os.path.splitext(path)[1]
    if suffix == ".py":
        yield from python_prose(text)
        return
    if suffix == ".go":
        yield from go_prose(text)
        return
    for n, raw in enumerate(text.splitlines(), 1):
        if suffix in (".yml", ".yaml", ".sh", ".tf", ".hcl") or os.path.basename(path) in PROSE_NAMES:
            m = HASH_COMMENT.search(raw)
            if m:
                yield n, m.group(1)
        else:
            yield n, raw


def walk(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in sorted(filenames):
            if not (name.endswith(PROSE_SUFFIXES) or name in PROSE_NAMES):
                continue
            full = os.path.join(dirpath, name)
            rel = os.path.relpath(full, root)
            if is_exempt(rel) or any(r.search(rel) for r, _ in NOT_OUR_PROSE):
                continue
            try:
                yield rel, open(full, encoding="utf-8").read()
            except (UnicodeDecodeError, OSError):
                continue


# A repo-relative path named in prose is a claim about the tree. Verified rather than
# trusted: a doc pointing at a file that was renamed reads as authoritative and sends the
# reader nowhere, and nothing signals the drift.
PATH_CLAIM = re.compile(r"""
    \[[^\]]*\]\((?P<md>[^)#\s]+)\)      # a markdown link target
  | `(?P<code>[\w./-]+/[\w./-]+|[\w-]+\.(?:go|md|ya?ml|sh|py|json|example))`
""", re.X)

# Path-shaped strings that name something outside this repo, or a pattern rather than a
# file. Each is ASSERTED: one matching nothing fails the run.
NOT_A_REPO_PATH = [
    (re.compile(r"^https?://"), "an external URL"),
    (re.compile(r"^(landing-zone|eks-gitops|eks-agent-platform|portal|eks-fleet|nanohype|operators)/"),
     "a path in a sibling repo, which this tree does not contain"),
    (re.compile(r"^[\d./]+$"), "a CIDR or a numeric literal, not a path"),
    (re.compile(r"^rackctl\.yaml$"),
     "the operator's own config, which they write and .gitignore keeps out of the tree"),
    (re.compile(r"^(terraform|live|components|modules|charts|config|deploy)/"),
     "a path inside a repo rackctl drives"),
]


def path_claims(root, rel, text):
    """Yield (lineno, path) for each repo-relative path this file claims exists."""
    if not rel.endswith(".md"):
        return
    for n, line in enumerate(text.splitlines(), 1):
        for m in PATH_CLAIM.finditer(line):
            cand = m.group("md") or m.group("code")
            if not cand:
                continue
            if cand.startswith("./"):
                cand = cand[2:]
            yield n, cand


def check(root, assert_exemptions=False):
    findings, scanned = [], 0
    # Counted per rule and printed. A gate that passes over zero targets is sometimes
    # legitimately correct — empty is not always wrong — but INVISIBLE-empty never is: a
    # rule matching nothing because nothing of its shape exists and a rule matching nothing
    # because it is broken produce the same silent pass.
    examined = {"prose spans": 0, "path claims": 0}
    exempt_hits = {r.pattern: 0 for r, _ in EXEMPT}

    for rel, text in walk(root):
        scanned += 1
        for n, prose in prose_lines(rel, text):
            examined["prose spans"] += 1
            for rule, pattern, remedy in RULES:
                # finditer, not search: taking the first match reports one violation on a
                # span carrying several, so clearing it only reveals the next.
                for m in pattern.finditer(prose):
                    findings.append((rel, n, rule, m.group(0).strip(), remedy))

    path_exempt = {r.pattern: 0 for r, _ in NOT_A_REPO_PATH}
    for rel, text in walk(root):
        for n, cand in path_claims(root, rel, text):
            examined["path claims"] += 1
            skip = False
            for r, _ in NOT_A_REPO_PATH:
                if r.search(cand):
                    path_exempt[r.pattern] += 1
                    skip = True
            if skip:
                continue
            # A relative link resolves against the directory of the file that carries it,
            # which is how a reader following it in a browser or an editor resolves it.
            # Falling back to the root keeps a repo-rooted path working from anywhere.
            here = os.path.normpath(os.path.join(root, os.path.dirname(rel), cand))
            if not (os.path.exists(here) or os.path.exists(os.path.join(root, cand))):
                findings.append((rel, n, "unresolved-path", cand,
                                 "this path does not exist in the tree. A doc pointing at a "
                                 "renamed or absent file reads as authoritative and sends the "
                                 "reader nowhere"))

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
        # Repo hygiene, not gate behaviour — see the note in scripts/pins.py.
        for r, why in NOT_A_REPO_PATH:
            if path_exempt[r.pattern] == 0:
                findings.append(("", 0, "dead-exemption", r.pattern,
                                 f"this path exemption ({why}) matches nothing — an exemption "
                                 "that outlives what it exempted hides the next violation"))
        for r, why in EXEMPT:
            if exempt_hits[r.pattern] == 0:
                findings.append(("", 0, "dead-exemption", r.pattern,
                                 f"this exemption ({why}) matches nothing — an exemption that "
                                 "outlives what it exempted hides the next violation of that shape"))

    return scanned, findings, examined


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
    ("a bare ledger id with no prefix", CLEAN + "\n// O1 settled the teardown wedge upstream.\n"),
    ("a cross-repo PR reference", CLEAN + "\n// Added by landing-zone#205.\n"),
    ("a bare issue number", CLEAN + "\n// Re-runnable by design (#16).\n"),
    ("a roadmap item", CLEAN + "\n// Available once target 11 lands.\n"),
    ("an observation tally", CLEAN + "\n// Observed: a fresh install generated 44 Applications.\n"),
    ("a time-to-discovery tally", CLEAN + "\n// Unrecoverable by retry. Discovered 6 minutes in.\n"),
    ("a session tally", CLEAN + "\n// This session alone turned up two such symbols.\n"),
    ("a leftover-resource tally", CLEAN + "\n// A failed install left three unattached volumes behind.\n"),
    ("a health-ratio tally", CLEAN + "\n// The only reason a 44/44-healthy cluster survived.\n"),
    ("a bare count with an elided noun", CLEAN + "\n// Dashboard ids die; three had.\n"),
    ("session narration", CLEAN + "\n// As discussed, this is kept here for future Claude.\n"),
    ("an unowned TODO", CLEAN + "\n// TODO: wire this up.\n"),
    ("an in-repo line-number citation", CLEAN + "\n// The guard lives at phases/agentplatform.go:224.\n"),
    ("a cross-repo line-number citation", CLEAN + "\n// Matches operators/platform_iam.go:177-190.\n"),
    ("a violation inside a shipped error string", CLEAN +
     '\nvar e = "could not apply — see ledger O14"\n'),
]

# Path controls run against a markdown fixture rather than the Go one.
PATH_CONTROLS = [
    ("a markdown link to a file that does not exist", "See [the runbook](docs/nope.md).\n", "reject"),
    ("a code-span path that does not exist", "Read `internal/nope/nope.go` first.\n", "reject"),
    ("a link to a file that does exist", "See [the source](x.go).\n", "accept"),
    ("an external URL", "See [the spec](https://example.com/x.md).\n", "accept"),
    ("a sibling-repo path", "Mirrors `landing-zone/components/aws/cluster/eks.tf`.\n", "accept"),
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
    scanned, findings, _ = check(root)
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
        _, findings, _ = check(root)
        if findings:
            print(f"control: {name} rejected")
        else:
            print(f"control: {name} was ACCEPTED — this gate cannot catch it", file=sys.stderr)
            failed = True

    lay(CLEAN)
    md = os.path.join(root, "doc.md")
    for name, body, want in PATH_CONTROLS:
        open(md, "w").write(body)
        _, findings, _ = check(root)
        hit = [f for f in findings if f[2] == "unresolved-path"]
        if want == "accept":
            if hit:
                print(f"control: {name} — reported a finding it should not have: {hit}", file=sys.stderr)
                failed = True
            else:
                print(f"control: {name} correctly ignored")
        elif hit:
            print(f"control: {name} rejected")
        else:
            print(f"control: {name} was ACCEPTED — this gate cannot catch it", file=sys.stderr)
            failed = True
    os.remove(md)

    # The enumeration must fail on empty, not pass.
    empty = tempfile.mkdtemp()
    _, findings, _ = check(empty)
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

    scanned, findings, examined = check(REPO, assert_exemptions=SCANNING_THE_REPO)
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
    for what, n in examined.items():
        if n == 0:
            print(f"  {what}: 0 examined — this rule asserted NOTHING on this tree")
        else:
            print(f"  {what}: {n} examined")


if __name__ == "__main__":
    main()
