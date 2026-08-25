#!/bin/sh
# Prove every gate in this repo can still reject.
#
# A check that cannot fail is worse than no check, because it reports success. There are
# two independent ways a gate reaches that state and neither is visible in its own output —
# both look exactly like a pass:
#
#   1. Its rejection path rots. The comparison stops comparing, the pattern stops matching,
#      and it returns 0 on everything. Each gate answers --self-test for this.
#   2. It still rejects, and the step is allowed to fail without failing the job.
#      continue-on-error and `|| true` both do that.
#
# This asserts both. It lives in scripts/ rather than inline in the workflow so the
# soft-fail scan is not its own input — an inline grep matches the line that defines it and
# reports a finding against itself, every run, forever.
set -e

WORKFLOWS="${WORKFLOWS:-.github/workflows}"
SCRIPTS="${SCRIPTS:-scripts}"

status=0

# ── 1. Each gate proves it rejects ──────────────────────────────────────────
#
# Discovered rather than listed: a gate added to scripts/ without a --self-test is exactly
# the half of this invariant that goes missing, so an absent one is a failure rather than
# something to remember to wire up.
for gate in "$SCRIPTS"/coverage.sh "$SCRIPTS"/pins.sh; do
  [ -f "$gate" ] || { echo "gates: $gate is missing" >&2; status=1; continue; }
  if ! grep -q -- '--self-test' "$gate"; then
    echo "gates: $gate has no --self-test, so nothing shows it can still reject" >&2
    status=1
    continue
  fi
  if sh "$gate" --self-test; then
    :
  else
    echo "gates: $gate --self-test failed" >&2
    status=1
  fi
done

# ── 2. No step can fail without failing the job ─────────────────────────────
soft="$(grep -nE 'continue-on-error|\|\|[[:space:]]*true' "$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml 2>/dev/null || true)"
if [ -n "$soft" ]; then
  echo "gates: a workflow step can fail without failing the job:" >&2
  echo "$soft" >&2
  status=1
else
  echo "gates: no workflow step soft-fails"
fi

exit "$status"
