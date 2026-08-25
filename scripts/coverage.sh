#!/bin/sh
# Enforce the org coverage floors (nanohype/standards/testing-rubric.json).
#
# The floor has to fail the build rather than be reported. A number nobody gates on only
# moves one way, and it falls fastest in the code least pleasant to test — which on an
# installer is the code that deletes things. Go ships `go test -coverprofile` and no
# threshold to go with it; this is the missing half.
#
# Two gates, and the second is the one that matters:
#
#   global    testing-rubric coverage_floor.statements. Catches a broad regression.
#   critical  security-critical-100. Catches the specific regression the global floor
#             cannot see, where overall coverage stays healthy while a sweep that issues
#             terminate-instances or delete-role loses its last test.
set -e

PROFILE="${PROFILE:-coverage.out}"

GLOBAL_FLOOR=75

# Functions that decide what gets destroyed, or that stand between a human and a teardown.
# security-critical-100 puts these at 100%, and they are held there by name rather than by
# file: the exported entry points beside them are one-line delegations to these cores,
# unreachable without a live account, and a file average would let a real gap hide behind
# them.
#
# A function added to a destructive path belongs on this list. That is part of writing the
# sweep, not a follow-up — an unlisted one is exactly the gap this gate exists to close.
CRITICAL_FUNCS="
internal/reap/reap.go:orphanedNodes
internal/reap/reap.go:orphanedVolumes
internal/reap/reap.go:reportUnprovableVolumes
internal/reap/reap.go:forceDeleteRole
internal/reap/own.go:Proves
internal/reap/own.go:roleTags
cmd/misc.go:confirmDestroy
internal/phases/agentplatform.go:createBucketArgs
"

go test -coverprofile="$PROFILE" -covermode=set ./... >/dev/null

report="$(go tool cover -func="$PROFILE")"
status=0

total="$(printf '%s\n' "$report" | awk '/^total:/ {gsub(/%/,"",$NF); print $NF}')"
if [ -z "$total" ]; then
  echo "coverage: could not read a total out of $PROFILE" >&2
  exit 1
fi

# awk compares as floats; the shell cannot.
if awk -v got="$total" -v want="$GLOBAL_FLOOR" 'BEGIN { exit !(got >= want) }'; then
  echo "coverage: ${total}% of statements (floor ${GLOBAL_FLOOR}%)"
else
  echo "coverage: ${total}% of statements is below the ${GLOBAL_FLOOR}% floor" >&2
  status=1
fi

for entry in $CRITICAL_FUNCS; do
  file="${entry%%:*}"
  func="${entry##*:}"

  # Match the function by name in its own file, so a same-named function elsewhere cannot
  # stand in for it — a silent substitution would report a pass for a function nobody tested.
  got="$(printf '%s\n' "$report" \
    | awk -v f="/$file:" -v n="$func" '$0 ~ f && $(NF-1) == n { gsub(/%/,"",$NF); print $NF; exit }')"

  if [ -z "$got" ]; then
    echo "coverage: $file:$func is on the destructive-path list and the profile does not name it — renamed, moved, or deleted without updating this list" >&2
    status=1
    continue
  fi
  if awk -v got="$got" 'BEGIN { exit !(got >= 100) }'; then
    echo "coverage: $file:$func 100%"
  else
    echo "coverage: $file:$func is ${got}%, and a function that decides what gets destroyed carries 100%" >&2
    status=1
  fi
done

exit "$status"
