#!/bin/sh
# The anti-vacuity floor: every gate in this repo proves it can reject, and nothing here
# can quietly shrink.
#
# A check that cannot fail is worse than no check, because it reports success. There are
# three independent ways a gate suite reaches that state and none is visible in its own
# output — all three look exactly like a pass:
#
#   1. A gate's rejection path rots. The comparison stops comparing, the pattern stops
#      matching, and it returns 0 on everything.
#   2. A gate ships without controls at all, or the suite stops running them.
#   3. A gate still rejects, and the step is allowed to fail without failing the job.
#      continue-on-error and `|| true` both do that.
#
# Against (1) and (2): gates are DISCOVERED rather than listed, each must carry positive
# controls, and each runs them. A gate added to scripts/ without controls fails this
# immediately rather than being something to remember to wire up. Discovering zero gates is
# itself a failure — a glob that matches nothing is how a walking check dies without a
# sound.
#
# Against (3): the workflows are scanned for soft-failing steps. This lives in scripts/
# rather than inline in a workflow so that scan is not its own input — an inline grep
# matches the line that defines it and reports a finding against itself, forever.
set -e

WORKFLOWS="${WORKFLOWS:-.github/workflows}"
SCRIPTS="${SCRIPTS:-scripts}"

status=0
gates=0

# ── 1. Every gate carries controls and passes them ──────────────────────────
#
# A gate is a script in scripts/ that is not itself a delivery artifact. install.sh ships
# to operators and is not a gate; anything else here is one.
for gate in "$SCRIPTS"/*.sh "$SCRIPTS"/*.py; do
  [ -f "$gate" ] || continue
  case "$(basename "$gate")" in
    install.sh) continue ;;             # shipped to operators, not a gate
    "$(basename "$0")") continue ;;     # this file
  esac
  gates=$((gates + 1))

  if ! grep -q 'control' "$gate"; then
    echo "gates: $gate ships no positive controls — a gate nothing proves can reject reports success forever" >&2
    status=1
    continue
  fi

  case "$gate" in
    *.py) cmd="python3 $gate --controls-only" ;;
    *)    cmd="sh $gate --controls-only" ;;
  esac
  if $cmd; then
    :
  else
    echo "gates: $gate failed its own controls" >&2
    status=1
  fi
done

if [ "$gates" -eq 0 ]; then
  echo "gates: no gates were discovered in $SCRIPTS — the enumeration matched nothing, which is how a suite reports success over zero checks" >&2
  status=1
else
  echo "gates: $gates gate(s) discovered, each proving it can reject"
fi

# ── 2. No step can fail without failing the job ─────────────────────────────
found=0
for wf in "$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml; do
  [ -f "$wf" ] || continue
  found=$((found + 1))
done
if [ "$found" -eq 0 ]; then
  echo "gates: no workflows found under $WORKFLOWS — nothing was actually scanned" >&2
  status=1
else
  soft="$(grep -nE 'continue-on-error|\|\|[[:space:]]*true' "$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml 2>/dev/null || true)"
  if [ -n "$soft" ]; then
    echo "gates: a workflow step can fail without failing the job:" >&2
    echo "$soft" >&2
    status=1
  else
    echo "gates: $found workflow(s) scanned, no step soft-fails"
  fi
fi

exit "$status"
