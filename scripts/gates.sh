#!/bin/sh
# The anti-vacuity floor: every gate in this repo proves it can reject, and nothing here
# can quietly shrink.
#
# A check that cannot fail is worse than no check, because it reports success. Four ways a
# gate suite reaches that state, none of them visible in its own output:
#
#   1. A gate's rejection path rots — the comparison stops comparing and it returns 0 on
#      everything.
#   2. A gate ships without controls, or the suite stops running them.
#   3. A gate still rejects, and its step is allowed to fail without failing the job.
#   4. The suite's own check for (2) is satisfied by TEXT rather than by behaviour. A gate
#      whose controls were deleted and replaced by a comment saying so passes a grep for
#      the word "control" — so this asserts what a gate DOES, never what its source says.
#
# Gates are DISCOVERED rather than listed, and discovering zero — or scanning zero
# workflows — is itself a failure. A glob that matches nothing is how a walking check dies
# without a sound.
#
# This lives in scripts/ rather than inline in a workflow so the soft-fail scan is not its
# own input: an inline grep matches the line that defines it and reports a finding against
# itself, forever.
set -e

WORKFLOWS="${WORKFLOWS:-.github/workflows}"
SCRIPTS="${SCRIPTS:-scripts}"

status=0
gates=0

# ── 1. Every gate demonstrates a rejection ──────────────────────────────────
#
# The evidence is the gate's own control output, and it has to show BOTH halves: a clean
# fixture accepted, and at least one violation rejected. A gate that rejects everything is
# as useless as one that rejects nothing, and either alone would pass a weaker check.
for gate in "$SCRIPTS"/*.sh "$SCRIPTS"/*.py; do
  [ -f "$gate" ] || continue
  case "$(basename "$gate")" in
    install.sh) continue ;;             # shipped to operators, not a gate
    gates.sh) continue ;;               # this file
  esac
  gates=$((gates + 1))

  case "$gate" in
    *.py) out="$(python3 "$gate" --controls-only 2>&1)" || { echo "gates: $gate failed its own controls" >&2; echo "$out" >&2; status=1; continue; } ;;
    *)    out="$(sh "$gate" --controls-only 2>&1)"      || { echo "gates: $gate failed its own controls" >&2; echo "$out" >&2; status=1; continue; } ;;
  esac

  rejected="$(printf '%s\n' "$out" | grep -c '^control: .* rejected$' || true)"
  accepted="$(printf '%s\n' "$out" | grep -c 'passes$' || true)"

  if [ "$rejected" -eq 0 ]; then
    echo "gates: $gate demonstrated no rejection — its controls were removed, or they have stopped rejecting" >&2
    status=1
  elif [ "$accepted" -eq 0 ]; then
    echo "gates: $gate never showed a clean fixture passing — a gate that rejects everything proves nothing" >&2
    status=1
  else
    echo "gates: $gate — $rejected control(s) rejected, clean fixture accepted"
  fi
done

if [ "$gates" -eq 0 ]; then
  echo "gates: no gates were discovered in $SCRIPTS — the enumeration matched nothing, which is how a suite reports success over zero checks" >&2
  status=1
fi

# ── 2. No step can fail without failing the job ─────────────────────────────
#
# Comment BODIES are blanked rather than the lines deleted, so a commented-out
# continue-on-error is not read as an active one and the reported line numbers stay true.
# This check is looking for a live setting, not for text that mentions one.
found=0
for wf in "$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml; do
  [ -f "$wf" ] || continue
  found=$((found + 1))
  soft="$(sed -E 's/(^|[[:space:]])#.*/\1#/' "$wf" | grep -nE 'continue-on-error|\|\|[[:space:]]*true' || true)"
  if [ -n "$soft" ]; then
    echo "gates: $wf has a step that can fail without failing the job:" >&2
    echo "$soft" >&2
    status=1
  fi
done
if [ "$found" -eq 0 ]; then
  echo "gates: no workflows found under $WORKFLOWS — nothing was actually scanned" >&2
  status=1
else
  echo "gates: $found workflow(s) scanned, no step soft-fails"
fi

exit "$status"
