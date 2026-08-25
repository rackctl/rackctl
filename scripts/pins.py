#!/usr/bin/env python3
"""Assert that every version pinned in this repo is watched by Renovate.

A description of what is covered is the thing that goes stale. A comment saying "the
workflows carry no other pins" is true when written, silently false at the first edit that
adds one, and there is no signal either way. So this asserts instead: it enumerates the
pins in the tree, applies the ACTUAL regexes from .github/renovate.json, and fails on any
pin no manager claims.

Four properties, each of which a simpler gate gets wrong:

  reads the config      The managers are parsed out of renovate.json and applied. A gate
                        carrying its own copy of the rule is a second source of truth: a
                        manager deleted upstream leaves the gate still asserting coverage
                        that no longer exists.
  per pin, not per file A pin is covered when a regex matches THAT line. Evaluated per
                        file, one watched pin vouches for every unwatched pin beside it.
  comments stripped     Matching is done on comment-stripped text. A gate that reads a
                        comment as content fails in exactly the case it exists for, because
                        the thing standing where a real pin should be is often a comment
                        saying so.
  fails on empty        Zero discovered pins is a failure, not a pass. A glob that matches
                        nothing is how a walking gate dies without a sound.

The positive controls run on EVERY invocation, not behind a flag. There is then no CI step
to forget and no flag a caller can silently omit, so the gate cannot drift from the
workflow that calls it.
"""

import json
import os
import re
import sys
import tempfile

REPO = os.environ.get("REPO_ROOT", ".")

# Repo-hygiene checks — a dead exemption, a manager matching nothing — are findings about
# THIS tree, not about whether the gate can reject. They run only against the default root.
# A fixture built to exercise one rule carries neither, so asserting them there would make
# every fixture fail and prove nothing about the gate.
SCANNING_THE_REPO = "REPO_ROOT" not in os.environ

# Version-shaped tokens that are NOT dependency pins. Each entry is asserted, not
# described: if it stops matching anything, this gate fails, so an exemption cannot outlive
# the thing it exempted.
NOT_A_PIN = [
    (r"fetch-depth:", "a git history depth, not a version"),
    (r"\b\d{1,3}(\.\d{1,3}){3}/\d{1,2}\b", "a CIDR block, not a version"),
]


def die(msg):
    print(f"pins: {msg}", file=sys.stderr)
    sys.exit(1)


def blank_comment_body(line):
    """Blank a trailing YAML comment's BODY, respecting quotes, keeping the '#'.

    One stripper, two views, chosen per check — reading one view for two purposes is how a
    gate goes blind:

      raw       when the thing being looked for IS an annotation. The `# v7.0.1` beside a
                SHA is not decoration, it is what Renovate rewrites, so the version-comment
                check reads the raw line.
      blanked   when a comment must not be able to satisfy a check looking for content. A
                commented-out `uses:` is not an action reference, and a customManager's
                regex must match a real pin rather than prose mentioning one.

    The body is blanked rather than the line deleted so the '#' survives and offsets stay
    put, which keeps reported line numbers true. Naive splitting on '#' would also cut a
    URL fragment or a quoted value in half, so quotes are respected.
    """
    out, quote = [], None
    i = 0
    while i < len(line):
        c = line[i]
        if quote:
            out.append(c)
            if c == "\\" and i + 1 < len(line):
                out.append(line[i + 1])
                i += 2
                continue
            if c == quote:
                quote = None
        elif c in "\"'":
            quote = c
            out.append(c)
        elif c == "#" and (i == 0 or line[i - 1] in " \t"):
            break
        else:
            out.append(c)
        i += 1
    return "".join(out) + ("#" if quote is None and "#" in line[len("".join(out)):] else "")


def to_python_re(pattern):
    """Renovate's regexes use JavaScript named groups; Python spells them differently.

    Translated rather than rewritten, so the gate applies the SAME expression Renovate
    will — a gate carrying its own paraphrase of the rule is a second source of truth.
    """
    return re.compile(re.sub(r"\(\?<(?![=!])", "(?P<", pattern))


def renovate_re(pattern):
    """Renovate wraps a file pattern in slashes when it is a regex; a bare string is a glob."""
    if pattern.startswith("/") and pattern.endswith("/") and len(pattern) > 1:
        return re.compile(pattern[1:-1])
    return re.compile(re.escape(pattern).replace(r"\*", ".*") + "$")


class Manager:
    def __init__(self, dep, file_res, match_res):
        self.dep, self.file_res, self.match_res = dep, file_res, match_res
        self.hits = 0

    def claims(self, path, line):
        if not any(r.search(path) for r in self.file_res):
            return False
        if any(r.search(line) for r in self.match_res):
            self.hits += 1
            return True
        return False


def load_managers(path):
    try:
        cfg = json.load(open(path))
    except FileNotFoundError:
        die(f"{path} is missing — nothing watches any pin")
    except json.JSONDecodeError as e:
        die(f"{path} is not valid JSON: {e}")

    managers = []
    for m in cfg.get("customManagers", []):
        pats = m.get("managerFilePatterns") or m.get("fileMatch") or []
        strings = m.get("matchStrings") or []
        if not pats or not strings:
            die(f"{path}: a customManager for {m.get('depNameTemplate','?')} has no file pattern or no matchStrings")
        managers.append(Manager(
            m.get("depNameTemplate", "?"),
            [renovate_re(p) for p in pats],
            [to_python_re(s) for s in strings],
        ))
    return cfg, managers


# ── pin discovery ────────────────────────────────────────────────────────────
#
# Deliberately broad. A shape nobody wrote a rule for is exactly the pin that goes
# unwatched, so the enumeration errs toward finding too much and the NOT_A_PIN list
# carries the justified exclusions — each of which is itself asserted.
VERSION_SHAPED = re.compile(r"(@v?\d+(\.\d+)*|==\d+(\.\d+)*|[\"']?~>\s*v\d+|:\s*[\"']?v?\d+\.\d+)")
# [ \t] rather than \s throughout: \s matches a newline, so an anchored pattern applied to
# joined text swallows the blanked lines above its target and reports the wrong line. The
# matching below is per-line, which makes that latent rather than live — the anchor is what
# would make it live, so it is written the safe way.
USES = re.compile(r"^[ \t]*-?[ \t]*uses:[ \t]*(\S+)")
SHA_PIN = re.compile(r"@[0-9a-f]{40}(\s|$)")
VERSION_COMMENT = re.compile(r"#[ \t]*v?\d+(\.\d+)*[ \t]*$")


# Files a pin can live in. Scanning only .github/workflows would make the PATH the oracle:
# the expectation "pins live here" would decide what gets read, so a pin added anywhere else
# is invisible — and invisible is the state this gate exists to prevent.
SCANNED_SUFFIXES = (".yml", ".yaml", ".sh")
SCANNED_NAMES = ("Makefile",)
SKIP_DIRS = {".git", "vendor", "node_modules", "dist", "bin"}


def scannable(root):
    """Yield (repo-relative path, absolute path) for every file a pin could live in."""
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in sorted(filenames):
            if name.endswith(SCANNED_SUFFIXES) or name in SCANNED_NAMES:
                full = os.path.join(dirpath, name)
                yield os.path.relpath(full, root), full


def discover(root):
    pins, exempt_hits = [], {p: 0 for p, _ in NOT_A_PIN}
    files = 0

    for path, full in scannable(root):
        files += 1
        try:
            lines = open(full, encoding="utf-8").readlines()
        except (UnicodeDecodeError, OSError):
            continue
        for n, raw in enumerate(lines, 1):
            line = blank_comment_body(raw).rstrip()
            if not line.strip():
                continue
            exempted = False
            for pat, _ in NOT_A_PIN:
                if re.search(pat, line):
                    exempt_hits[pat] += 1
                    exempted = True
            if exempted:
                continue
            if USES.match(line):
                pins.append(("action", path, n, raw.rstrip()))
            elif VERSION_SHAPED.search(line):
                pins.append(("value", path, n, raw.rstrip()))

    gomod = os.path.join(root, "go.mod")
    if os.path.isfile(gomod):
        for n, raw in enumerate(open(gomod), 1):
            if re.match(r"^[ \t]+\S+/\S+ v\d", raw):
                pins.append(("gomod", "go.mod", n, raw.rstrip()))

    return pins, exempt_hits, files


def check(root, renovate_path, assert_exemptions=False):
    cfg, managers = load_managers(renovate_path)
    pins, exempt_hits, files = discover(root)
    problems = []

    if not pins:
        problems.append("no pins were discovered at all — the enumeration matched nothing, "
                        "which is how a walking gate reports success over zero files")

    gomod_watched = "gomodTidy" in json.dumps(cfg) or any(
        "gomod" in json.dumps(cfg.get("packageRules", []))
        for _ in [0]) or "config:recommended" in cfg.get("extends", [])

    counts = {"action": 0, "value": 0, "gomod": 0, "files": files}
    for kind, path, n, raw in pins:
        counts[kind] += 1
        line = blank_comment_body(raw)

        if kind == "gomod":
            if not gomod_watched:
                problems.append(f"{path}:{n}: go.mod is not covered — {renovate_path} does not enable the gomod manager")
            continue

        if kind == "action":
            ref = USES.match(line).group(1)
            if not SHA_PIN.search(ref + " "):
                problems.append(f"{path}:{n}: {ref} is pinned to a mutable tag, not a commit SHA — "
                                "whoever can move the tag changes what runs here")
            elif not VERSION_COMMENT.search(raw.rstrip()):
                problems.append(f"{path}:{n}: {ref} is a SHA with no trailing version comment "
                                "(# vN.N.N) — Renovate cannot tell what version the SHA currently is")
            continue

        # RAW, not blanked. The question here is "would Renovate match this line", and
        # Renovate reads the whole file — so a manager whose regex legitimately matches
        # inside a comment is LIVE, and asking the blanked view would declare it dead. The
        # view has to be the consumer's, not the one that is convenient here.
        if not any(m.claims(path, raw.rstrip()) for m in managers):
            problems.append(f"{path}:{n}: no customManager in {renovate_path} matches this pin:\n"
                            f"      {raw.strip()}")

    # Always. A manager matching nothing is a statement about whatever tree is being
    # scanned, which for a control fixture is that fixture — gating it on the real repo
    # would disable the control that proves this check works.
    for m in managers:
        if m.hits == 0:
            problems.append(f"{renovate_path}: the customManager for {m.dep} matches nothing in the tree — "
                            "a manager watching a pin that no longer exists is coverage on paper only")

    # Repo hygiene, not gate behaviour: a fixture built to exercise one rule carries none
    # of the shapes these exempt, so asserting them there would fail every fixture and
    # prove nothing about the gate.
    for pat, why in NOT_A_PIN if assert_exemptions else []:
        if exempt_hits[pat] == 0:
            problems.append(f"the NOT_A_PIN exemption {pat!r} ({why}) matches nothing — "
                            "an exemption that outlives what it exempted hides the next pin of that shape")

    return counts, problems, managers


# ── positive controls ────────────────────────────────────────────────────────
#
# Each control introduces the exact violation its check exists to catch, and the gate must
# exit non-zero on it. Each asserts the fixture is CLEAN FIRST: without that, a non-zero
# exit proves nothing, because the gate might have been failing for an unrelated reason all
# along.
CLEAN_RENOVATE = {
    "extends": ["config:recommended"],
    "postUpdateOptions": ["gomodTidy"],
    "customManagers": [{
        "customType": "regex",
        "managerFilePatterns": ["/^\\.github/workflows/.+\\.ya?ml$/"],
        "matchStrings": ["go-version:\\s*\"(?<currentValue>\\d+\\.\\d+)\""],
        "depNameTemplate": "go",
        "datasourceTemplate": "golang-version",
    }],
}

CLEAN_WORKFLOW = """\
jobs:
  build:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: "1.26"
          apiVersion: v1
      - run: golangci-lint run
        # version: 2
"""

CLEAN_GOMOD = "module x\n\ngo 1.26\n\nrequire (\n\tgithub.com/spf13/cobra v1.10.2\n)\n"

CONTROLS = [
    ("an action on a mutable tag",
     lambda wf, rn: (wf.replace("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
                                "actions/checkout@v4"), rn)),
    ("a SHA pin whose only comment is not a version",
     lambda wf, rn: (wf.replace("# v7.0.1", "# vendored, do not touch"), rn)),
    ("a SHA pin with no comment at all",
     lambda wf, rn: (wf.replace(" # v7.0.1", ""), rn)),
    ("a value pin no customManager matches",
     lambda wf, rn: (wf, {**rn, "customManagers": []})),
    ("a customManager that matches nothing in the tree",
     lambda wf, rn: (wf.replace('go-version: "1.26"', "go-version-removed: x"), rn)),
    ("go.mod with the gomod manager not enabled",
     lambda wf, rn: (wf, {k: v for k, v in rn.items() if k not in ("postUpdateOptions", "extends")})),
    ("an empty enumeration",
     lambda wf, rn: ("", rn)),
    # The blanked view: a commented-out action reference is not an action reference. Read
    # raw, this line would be counted as a pin and then rejected for the mutable tag it
    # carries — a false finding, and evidence the gate is reading comments as content.
    ("a commented-out action read as a real one",
     lambda wf, rn: (wf + "      # - uses: actions/stale@v1\n", rn),
     "accept"),
]

# Blanked comment lines above a violation must not shift the line it is reported at. An
# anchored \s* would swallow them and cite the wrong line, and nothing about that failure
# announces itself — the gate still rejects, at a line that was never the pin.
LINE_FIDELITY = """\
# RACKCTL-CTL-PADDING-A
# RACKCTL-CTL-PADDING-B
jobs:
  build:
    steps:
      # RACKCTL-CTL-PADDING-C
      - uses: actions/checkout@v4
"""


def run_controls():
    if not CONTROLS:
        die("this gate ships no positive controls — a gate nothing proves can reject reports success forever")

    root = tempfile.mkdtemp()
    wf_dir = os.path.join(root, ".github", "workflows")
    os.makedirs(wf_dir)

    def lay(workflow, renovate):
        for f in os.listdir(wf_dir):
            os.remove(os.path.join(wf_dir, f))
        if workflow:
            open(os.path.join(wf_dir, "ci.yml"), "w").write(workflow)
        open(os.path.join(root, "go.mod"), "w").write(CLEAN_GOMOD)
        rn = os.path.join(root, "renovate.json")
        json.dump(renovate, open(rn, "w"))
        return rn

    # Anti-vacuity: the clean fixture must PASS before any mutation is believed.
    rn = lay(CLEAN_WORKFLOW, CLEAN_RENOVATE)
    _, problems, _ = check(root, rn)
    if problems:
        die("the clean control fixture does not pass, so no rejection below proves anything:\n  "
            + "\n  ".join(problems))
    print("control: a clean tree passes")

    failed = False
    for entry in CONTROLS:
        name, mutate = entry[0], entry[1]
        want = entry[2] if len(entry) > 2 else "reject"
        wf, rnj = mutate(CLEAN_WORKFLOW, CLEAN_RENOVATE)
        rn = lay(wf, rnj)
        _, problems, _ = check(root, rn)
        if want == "accept":
            if problems:
                print(f"control: {name} — the gate reported a finding it should not have:", file=sys.stderr)
                for p in problems:
                    print(f"    {p}", file=sys.stderr)
                failed = True
            else:
                print(f"control: {name} correctly ignored")
        elif problems:
            print(f"control: {name} rejected")
        else:
            print(f"control: {name} was ACCEPTED — this gate cannot catch it", file=sys.stderr)
            failed = True
    # The stripper must preserve the line COUNT, not merely the numbers of the lines this
    # fixture happens to check. A refactor from blanking a comment's body to deleting the
    # line passes an off-by-one assertion on a fixture with one comment and fails here.
    joined = "\n".join(blank_comment_body(l) for l in LINE_FIDELITY.splitlines())
    if len(joined.splitlines()) != len(LINE_FIDELITY.splitlines()):
        print("control: the stripper does not preserve the line count — every citation "
              "below a comment is shifted", file=sys.stderr)
        failed = True
    else:
        print("control: the stripper preserves the line count")

    # Line fidelity, checked against a fixture whose violation sits at a line number no
    # off-by-N could reach by accident.
    lay(LINE_FIDELITY, CLEAN_RENOVATE)
    _, problems, _ = check(root, os.path.join(root, "renovate.json"))
    mutable = [p for p in problems if "mutable tag" in p]
    if len(mutable) != 1:
        print(f"control: line fidelity — expected exactly one mutable-tag finding, got {mutable}", file=sys.stderr)
        failed = True
    elif ":7:" not in mutable[0]:
        print(f"control: the violation is on line 7 and was reported elsewhere — blanked lines "
              f"above it shifted the citation: {mutable[0]}", file=sys.stderr)
        failed = True
    else:
        print("control: a violation under blanked comment lines is cited at its own line")

    if failed:
        sys.exit(1)


def main():
    run_controls()
    if "--controls-only" in sys.argv:
        return

    counts, problems, managers = check(REPO, os.path.join(REPO, ".github", "renovate.json"),
                                       assert_exemptions=SCANNING_THE_REPO)
    if problems:
        print("pins: unwatched or unverifiable version pins:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)

    print(f"pins: {counts['action']} action ref(s), {counts['value']} value pin(s), "
          f"{counts['gomod']} module(s) across {counts['files']} scanned file(s) — all watched")
    for m in managers:
        print(f"  {m.dep}: {m.hits} pin(s) matched")


if __name__ == "__main__":
    main()
