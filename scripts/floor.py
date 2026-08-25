#!/usr/bin/env python3
"""The anti-vacuity floor: prove every gate rejects, by OBSERVING it, never by reading it.

A gate that reports its own verdict is testimony. This floor SUPPLIES a known-bad input,
runs the gate against it, and reads the exit status — nothing the gate prints is consulted,
so a gate cannot pass by claiming to work.

The distinction is not academic. A previous version of this floor grepped each gate's
stdout for `control: … rejected`, and a four-line script that printed those words and
exited 0 passed it with zero checks behind it. Any floor consuming a count, a list of
lines, or a structured object the gate populates has the same hole in a different shape:
all of them are the subject describing itself.

Two runs per gate, not one. A known-good input must be ACCEPTED and a known-bad input
REJECTED, and because they are separate invocations against separate fixtures the two
halves cannot be satisfied by the same evidence — a gate that rejects everything fails the
good fixture, and one that accepts everything fails the bad one.

FIXTURES ARE OWNED HERE. A gate does not supply the input that tests it, for the same
reason it does not supply the verdict.

WHICH TREE THIS GRADES: the WORKING tree. Fixtures are built from literals in this file,
and each gate is invoked by path out of scripts/ as it exists on disk — nothing is
materialised from the git index. So an uncommitted edit to a gate is seen here, which is
the direction wanted while editing, and it means a green floor is a statement about the
working tree rather than about HEAD. Checking the committed tree is a separate act:
`git archive HEAD | tar -x -C <dir>` and run the gates there.

WHAT THIS CANNOT DO: probe itself. The rules below are exercised — the crash rule has a
control built to defeat the other two — but nothing here would notice if this file stopped
discovering gates in a way its own checks do not cover. That is its standing blind spot.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from gatelib import blank_comment_body  # noqa: E402

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPTS = os.path.join(REPO, "scripts")

# Scripts in scripts/ that are not gates. Each needs a reason, and the list is asserted:
# an entry naming a file that no longer exists fails the run, so it cannot outlive what it
# excused.
# The status check the default-branch ruleset requires, by name. Recorded here because the
# ruleset lives on the forge rather than in the tree: a check name that drifts from it stops
# being consulted at the merge decision, in whichever direction it drifts. A required check
# with no matching job blocks every merge; a job with no matching requirement blocks none.
REQUIRED_CHECK = "build"

NOT_A_GATE = {
    "crasher.py": "the floor's own control: a deliberately crashing gate, used to prove the "
                  "crash check below can still fire",
    "install.sh": "shipped to operators, not a gate",
    "install_test.sh": "the harness that RUNS install.sh; it supplies its own fixtures and "
                       "makes no verdict about the repo",
    "floor.py": "this file — the floor cannot probe itself, which is its own blind spot",
    "gatelib.py": "helpers the gates import; it makes no verdict of its own",
}

# Ways a workflow step can fail without failing the job. Enumerated by PROBE, not by
# reasoning: `continue-on-error` and `|| true` were the two this scan originally knew, and
# running the others against it showed each slipping through.
SOFT_FAIL = (
    "continue-on-error",
    "|| true",
    "||true",
    "|| :",
    "||:",
    "|| exit 0",
    "set +e",
)


# A CRASH IS NOT A REJECTION. A gate that raises exits non-zero and prints a traceback, and
# a floor reading only the status records that as a successful catch — exit-code-conflates-
# causes occurring inside the thing built to check for it.
#
# Worse together with the marker rule than either alone: a gate that crashes ONLY on the bad
# fixture, with an exception naming the planted file, satisfies BOTH. scripts/crasher.py is
# exactly that gate, kept as this check's control.
CRASH_MARKERS = (
    "Traceback (most recent call last)",
    "SyntaxError", "IndentationError", "ImportError", "ModuleNotFoundError",
    "panic:", "fatal error:", "Segmentation fault",
    "command not found", "No such file or directory",
    ": line ",          # a shell script dying mid-statement
)


# Statuses that mean the shell could not RUN the gate, so no verdict was reached. A floor
# reading "non-zero means it rejected" scores these as catches, and a gate whose tool went
# missing then reports as the strictest gate in the suite.
NOT_A_VERDICT = {126: "found but not executable", 127: "not found"}


def crashed(out, rc=None):
    """Did the gate fail to reach a verdict, rather than reach one?

    Two rules, because either alone is escapable. The text rule catches a gate that died
    mid-run and said so. The NUMBER catches the case the text rule cannot see at all: a gate
    that exits 127 having written nothing, because it discarded its diagnostics or its tool
    produced none. There is no string to match, and the status is indistinguishable from a
    rejection except by its value.
    """
    if rc in NOT_A_VERDICT:
        return True
    return any(m in out for m in CRASH_MARKERS)


def run(cmd, cwd=None, env=None):
    """Run a gate and return (exit status, combined output).

    THE VERDICT COMES FROM THE EXIT STATUS. The output is used for exactly one thing: to
    confirm a rejection NAMES the violation this floor planted. That is not the gate
    describing its own success — a gate is never asked whether it passed, and one that
    exits 0 fails here no matter what it prints. It closes the gap between "it failed" and
    "it found what I planted": a gate can reject a bad fixture for an unrelated reason —
    an empty enumeration, a missing file, a parse error — and a floor reading only the
    status would score that as proof it catches the planted thing.
    """
    e = dict(os.environ)
    if env:
        e.update(env)
    p = subprocess.run(cmd, cwd=cwd, env=e, stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True)
    return p.returncode, p.stdout or ""


# ── fixtures, one good and one bad per gate ─────────────────────────────────

def profile(entries, total):
    lines = [f"github.com/rackctl/rackctl/{f}:1:\t{fn}\t{pct}" for f, fn, pct in entries]
    lines.append(f"total:\t\t\t(statements)\t{total}")
    return "\n".join(lines) + "\n"


CRITICAL = [
    ("internal/reap/reap.go", "orphanedNodes"),
    ("internal/reap/reap.go", "orphanedVolumes"),
    ("internal/reap/reap.go", "reportUnprovableVolumes"),
    ("internal/reap/reap.go", "forceDeleteRole"),
    ("internal/reap/own.go", "Proves"),
    ("internal/reap/own.go", "roleTags"),
    ("cmd/misc.go", "confirmDestroy"),
    ("internal/phases/agentplatform.go", "createBucketArgs"),
]


def coverage_fixtures(d):
    good = os.path.join(d, "good.out")
    bad = os.path.join(d, "bad.out")
    open(good, "w").write(profile([(f, fn, "100.0%") for f, fn in CRITICAL], "88.8%"))
    # Known-bad: a destructive-path function below 100%. A gate that does not reject this
    # is not enforcing security-critical-100 whatever it says about itself.
    entries = [(f, fn, "100.0%") for f, fn in CRITICAL]
    entries[0] = (entries[0][0], entries[0][1], "13.37%")
    open(bad, "w").write(profile(entries, "88.8%"))
    return (["sh", os.path.join(SCRIPTS, "coverage.sh"), "--check-report", good], None, None), \
           (["sh", os.path.join(SCRIPTS, "coverage.sh"), "--check-report", bad], None, None), \
           "orphanedNodes"


RENOVATE = {
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

WF_GOOD = """\
jobs:
  build:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: "1.26"
          fetch-depth: 0
"""

# Known-bad: an action on a mutable tag. Whoever can move the tag changes what runs.
WF_BAD = WF_GOOD.replace(
    "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
    "actions/checkout@v4")

GOMOD = "module x\n\ngo 1.26\n\nrequire (\n\tgithub.com/spf13/cobra v1.10.2\n)\n"


def pins_fixtures(d):
    def tree(name, wf):
        root = os.path.join(d, name)
        os.makedirs(os.path.join(root, ".github", "workflows"), exist_ok=True)
        open(os.path.join(root, ".github", "workflows", "ci.yml"), "w").write(wf)
        json.dump(RENOVATE, open(os.path.join(root, ".github", "renovate.json"), "w"))
        open(os.path.join(root, "go.mod"), "w").write(GOMOD)
        return root
    cmd = ["python3", os.path.join(SCRIPTS, "pins.py")]
    return (cmd, None, {"REPO_ROOT": tree("pins-good", WF_GOOD)}), \
           (cmd, None, {"REPO_ROOT": tree("pins-bad", WF_BAD)}), \
           "actions/checkout@v4"


PROSE_GOOD = """\
// Package x does a thing.
//
// A value must fail rather than silently resolve: a fallback here would be reachable
// exactly when the guarantee is absent.
package x
"""

# Known-bad: an internal ledger reference the reader cannot resolve.
PROSE_BAD = PROSE_GOOD + "\n// Settled upstream. Ledger O27.\n"


def prose_fixtures(d):
    def tree(name, body):
        root = os.path.join(d, name)
        os.makedirs(root, exist_ok=True)
        open(os.path.join(root, "x.go"), "w").write(body)
        return root
    cmd = ["python3", os.path.join(SCRIPTS, "prose.py")]
    return (cmd, None, {"REPO_ROOT": tree("prose-good", PROSE_GOOD)}), \
           (cmd, None, {"REPO_ROOT": tree("prose-bad", PROSE_BAD)}), \
           "Ledger O27"


FIXTURES = {
    "coverage.sh": coverage_fixtures,
    "pins.py": pins_fixtures,
    "prose.py": prose_fixtures,
}


def self_check(d):
    """Prove the crash rule can still fire, using a gate built to defeat the other two.

    The floor cannot probe itself as a whole — that is its standing blind spot — but a rule
    it can exercise is a rule that has been shown to work rather than read.
    """
    crasher = os.path.join(SCRIPTS, "crasher.py")
    if not os.path.exists(crasher):
        print(f"floor: {crasher} is missing — the crash rule has no control, so nothing "
              "shows it can still fire", file=sys.stderr)
        return 1
    good = os.path.join(d, "sc-good"); os.makedirs(good, exist_ok=True)
    bad = os.path.join(d, "sc-bad"); os.makedirs(bad, exist_ok=True)
    open(os.path.join(bad, "BAD"), "w").write("x")

    rc_good, out_good = run(["python3", crasher], None, {"REPO_ROOT": good})
    rc_bad, out_bad = run(["python3", crasher], None, {"REPO_ROOT": bad})

    if crashed(out_good, rc_good) or rc_good != 0:
        print("floor: the crash control does not pass its own good fixture", file=sys.stderr)
        return 1
    if not crashed(out_bad, rc_bad):
        print("floor: the crash control crashed and this floor did not notice — the crash "
              "rule has stopped firing", file=sys.stderr)
        return 1
    if rc_bad == 0 or "Ledger O27" not in out_bad:
        print("floor: the crash control no longer defeats the status and marker rules, so "
              "it is not testing what it was built to test", file=sys.stderr)
        return 1
    # The second shape: 127 with NO output. The text rule cannot see it — there is no text.
    rc_q, out_q = run(["python3", crasher], None,
                      {"REPO_ROOT": bad, "CRASHER_MODE": "silent127"})
    if out_q.strip():
        print("floor: the silent-127 control printed something, so it is no longer testing "
              "the case a text rule cannot see", file=sys.stderr)
        return 1
    if rc_q not in NOT_A_VERDICT:
        print(f"floor: the silent-127 control exited {rc_q}, not 127 — it is not testing "
              "what it was built to test", file=sys.stderr)
        return 1
    if not crashed(out_q, rc_q):
        print("floor: a gate that exited 127 having written nothing was scored as a "
              "rejection — the number rule has stopped firing", file=sys.stderr)
        return 1
    print("floor: crash rule fires on a gate built to defeat the status and marker rules, "
          "and on one that exits 127 in silence")
    return 0


def main():
    status = 0
    d = tempfile.mkdtemp()
    status |= self_check(d)

    # ── every gate is discovered, and every one must have a fixture pair ─────
    discovered = sorted(
        f for f in os.listdir(SCRIPTS)
        if (f.endswith(".sh") or f.endswith(".py")) and f not in NOT_A_GATE
    )
    if not discovered:
        print("floor: no gates discovered — a walk that matches nothing reports success "
              "over zero checks", file=sys.stderr)
        sys.exit(1)

    for name, why in NOT_A_GATE.items():
        if not os.path.exists(os.path.join(SCRIPTS, name)):
            print(f"floor: the NOT_A_GATE entry {name!r} ({why}) names a file that does not "
                  "exist — an exemption cannot outlive what it excused", file=sys.stderr)
            status = 1

    for name in discovered:
        if name not in FIXTURES:
            print(f"floor: {name} has no known-good/known-bad fixture pair here. A gate is "
                  "proven by being fed a bad input and watched to reject it; nothing it "
                  "prints about itself is evidence.", file=sys.stderr)
            status = 1
            continue

        (good_cmd, good_cwd, good_env), (bad_cmd, bad_cwd, bad_env), marker = FIXTURES[name](d)

        rc, good_out = run(good_cmd, good_cwd, good_env)
        if crashed(good_out, rc):
            print(f"floor: {name} CRASHED on a known-good input — a gate that cannot run is "
                  "not a gate that passes", file=sys.stderr)
            status = 1
            continue
        if rc != 0:
            print(f"floor: {name} REJECTED a known-good input — a gate that fails everything "
                  "fails nothing", file=sys.stderr)
            status = 1
            continue

        rc, out = run(bad_cmd, bad_cwd, bad_env)
        if crashed(out, rc):
            print(f"floor: {name} CRASHED on the known-bad input rather than rejecting it. A "
                  "crash exits non-zero and can even name the planted violation in its "
                  "exception, so it satisfies both the status and the marker check while "
                  "performing no check at all. A status of 126 or 127 counts here too: the "
                  "shell could not run the gate, and no verdict was reached.", file=sys.stderr)
            status = 1
            continue
        if rc == 0:
            print(f"floor: {name} ACCEPTED a known-bad input — it cannot reject", file=sys.stderr)
            status = 1
            continue
        # The rejection must NAME the planted violation. Rejecting for an unrelated reason
        # — an empty enumeration, a missing file, a parse error — looks identical in the
        # exit status, and scoring that as proof credits the gate with catching something
        # it never saw.
        if marker not in out:
            print(f"floor: {name} rejected the known-bad input but never named {marker!r} — "
                  "it failed for some other reason, so this proves nothing about the "
                  "violation that was planted", file=sys.stderr)
            status = 1
            continue
        print(f"floor: {name} accepted a known-good input and rejected a known-bad one, "
              f"naming {marker!r}")

    # ── no workflow step may fail without failing the job ───────────────────
    #
    # Overridable so this check is itself probeable. A check that can only be run against
    # the real tree cannot be shown to reject: the real tree is, by construction, the one
    # case where it passes.
    wf_dir = os.environ.get("WORKFLOWS", os.path.join(REPO, ".github", "workflows"))
    wfs = sorted(f for f in os.listdir(wf_dir) if f.endswith((".yml", ".yaml")))
    if not wfs:
        print(f"floor: no workflows under {wf_dir} — nothing was scanned", file=sys.stderr)
        status = 1
    for wf in wfs:
        for n, raw in enumerate(open(os.path.join(wf_dir, wf)), 1):
            # Comment bodies blanked through the SHARED stripper: a commented-out
            # continue-on-error is not an active setting, and this check is looking for a
            # live one. Quote-aware, so a `|| true` inside a quoted string is not read as
            # a shell operator.
            line = blank_comment_body(raw)
            for shape in SOFT_FAIL:
                if shape in line:
                    print(f"floor: {wf}:{n} can fail without failing the job ({shape})",
                          file=sys.stderr)
                    status = 1
                    break
    if wfs and status == 0:
        print(f"floor: {len(wfs)} workflow(s) scanned, no step soft-fails")

    # ── every blocking check must live inside the ONE required job ──────────
    #
    # The ruleset on the default branch requires a single status check by name. A SECOND
    # job in the pull-request workflow would run, could go red, and would not block the
    # merge — and at the point of decision a red job that blocks nothing looks exactly like
    # a passing one. New checks therefore go in as STEPS of the required job. A genuinely
    # separate job has to be added to the ruleset at the same time, which is a change
    # outside this repository, so the gate names it rather than assuming it.
    ci = os.path.join(wf_dir, "ci.yml")
    if os.path.exists(ci):
        jobs, in_jobs = [], False
        for raw in open(ci):
            line = blank_comment_body(raw).rstrip()
            if line == "jobs:":
                in_jobs = True
                continue
            if in_jobs and line and not line[0].isspace():
                break
            if in_jobs and re.match(r"^  [\w-]+:$", line):
                jobs.append(line.strip().rstrip(":"))
        if jobs != [REQUIRED_CHECK]:
            print(f"floor: {os.path.basename(ci)} declares jobs {jobs}, but the default-branch "
                  f"ruleset requires exactly [{REQUIRED_CHECK!r}]. A job outside that set can "
                  f"fail without blocking a merge — add the check as a step of "
                  f"{REQUIRED_CHECK!r}, or add the new job to the ruleset.", file=sys.stderr)
            status = 1
        else:
            print(f"floor: every blocking check runs inside the required {REQUIRED_CHECK!r} job")

    shutil.rmtree(d, ignore_errors=True)
    sys.exit(status)


if __name__ == "__main__":
    main()
