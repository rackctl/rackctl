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

# Version-shaped tokens that are NOT dependency pins. Each entry is asserted, not
# described: if it stops matching anything, this gate fails, so an exemption cannot outlive
# the thing it exempted.
NOT_A_PIN = [
    (r"fetch-depth:", "a git history depth, not a version"),
]


def die(msg):
    print(f"pins: {msg}", file=sys.stderr)
    sys.exit(1)


def strip_yaml_comments(line):
    """Remove a trailing YAML comment, respecting quotes.

    Naive splitting on '#' would cut a URL fragment or a quoted value in half, and leaving
    comments in would let one satisfy a check that is looking for content.
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
    return "".join(out)


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
USES = re.compile(r"^\s*-?\s*uses:\s*(\S+)")
SHA_PIN = re.compile(r"@[0-9a-f]{40}(\s|$)")
VERSION_COMMENT = re.compile(r"#\s*v?\d+(\.\d+)*\s*$")


def discover(root):
    pins, exempt_hits = [], {p: 0 for p, _ in NOT_A_PIN}
    wf_dir = os.path.join(root, ".github", "workflows")

    if os.path.isdir(wf_dir):
        for name in sorted(os.listdir(wf_dir)):
            if not name.endswith((".yml", ".yaml")):
                continue
            path = os.path.join(".github", "workflows", name)
            for n, raw in enumerate(open(os.path.join(wf_dir, name)), 1):
                line = strip_yaml_comments(raw).rstrip()
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
            if re.match(r"^\s+\S+/\S+ v\d", raw):
                pins.append(("gomod", "go.mod", n, raw.rstrip()))

    return pins, exempt_hits


def check(root, renovate_path, assert_exemptions=False):
    cfg, managers = load_managers(renovate_path)
    pins, exempt_hits = discover(root)
    problems = []

    if not pins:
        problems.append("no pins were discovered at all — the enumeration matched nothing, "
                        "which is how a walking gate reports success over zero files")

    gomod_watched = "gomodTidy" in json.dumps(cfg) or any(
        "gomod" in json.dumps(cfg.get("packageRules", []))
        for _ in [0]) or "config:recommended" in cfg.get("extends", [])

    counts = {"action": 0, "value": 0, "gomod": 0}
    for kind, path, n, raw in pins:
        counts[kind] += 1
        line = strip_yaml_comments(raw)

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

        # A value pin is watched only when a real customManager regex matches THIS line.
        if not any(m.claims(path, line) for m in managers):
            problems.append(f"{path}:{n}: no customManager in {renovate_path} matches this pin:\n"
                            f"      {raw.strip()}")

    for m in managers:
        if m.hits == 0:
            problems.append(f"{renovate_path}: the customManager for {m.dep} matches nothing in the tree — "
                            "a manager watching a pin that no longer exists is coverage on paper only")

    for pat, why in NOT_A_PIN if assert_exemptions else []:
        if exempt_hits[pat] == 0:
            problems.append(f"the NOT_A_PIN exemption {pat!r} ({why}) matches nothing — "
                            "an exemption that outlives what it exempted hides the next pin of that shape")

    return counts, problems


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
]


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
    _, problems = check(root, rn)
    if problems:
        die("the clean control fixture does not pass, so no rejection below proves anything:\n  "
            + "\n  ".join(problems))
    print("control: a clean tree passes")

    failed = False
    for name, mutate in CONTROLS:
        wf, rnj = mutate(CLEAN_WORKFLOW, CLEAN_RENOVATE)
        rn = lay(wf, rnj)
        _, problems = check(root, rn)
        if problems:
            print(f"control: {name} rejected")
        else:
            print(f"control: {name} was ACCEPTED — this gate cannot catch it", file=sys.stderr)
            failed = True
    if failed:
        sys.exit(1)


def main():
    run_controls()
    if "--controls-only" in sys.argv:
        return

    counts, problems = check(REPO, os.path.join(REPO, ".github", "renovate.json"),
                             assert_exemptions=True)
    if problems:
        print("pins: unwatched or unverifiable version pins:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)

    print(f"pins: {counts['action']} action ref(s), {counts['value']} value pin(s), "
          f"{counts['gomod']} module(s) — all watched")


if __name__ == "__main__":
    main()
