#!/usr/bin/env python3
"""A gate that CRASHES on the bad fixture with a message naming the planted violation.

Constructed to defeat exit-status and name-the-mutation simultaneously: it exits non-zero
(the traceback does), and its output contains the marker. A floor checking only those two
records it as a clean catch, while the gate performed no check at all.
"""
import os
import sys

root = os.environ.get("REPO_ROOT", ".")
if os.path.exists(os.path.join(root, "BAD")):
    raise KeyError("Ledger O27")   # names the marker, exits non-zero
print("crasher: nothing to report")
sys.exit(0)
