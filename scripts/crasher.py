#!/usr/bin/env python3
"""A gate that CRASHES on the bad fixture with a message naming the planted violation.

Constructed to defeat exit-status and name-the-mutation simultaneously: it exits non-zero
(the traceback does), and its output contains the marker. A floor checking only those two
records it as a clean catch, while the gate performed no check at all.

CRASHER_MODE=silent127 is the second shape, and it defeats a third rule. A gate whose
interpreter or tool is absent exits 127 having evaluated nothing, and 127 is a status a
floor reads as a rejection. Recognising it by its message is not enough: a gate that
discards its diagnostics, or a tool that writes none, exits 127 SILENTLY, and there is no
text for a text rule to match. Only the number distinguishes "the shell could not run this"
from "this ran and rejected".
"""
import os
import sys

root = os.environ.get("REPO_ROOT", ".")
if os.path.exists(os.path.join(root, "BAD")):
    if os.environ.get("CRASHER_MODE") == "silent127":
        sys.exit(127)              # no output at all: nothing for a text rule to match
    raise KeyError("Ledger O27")   # names the marker, exits non-zero
print("crasher: nothing to report")
sys.exit(0)
