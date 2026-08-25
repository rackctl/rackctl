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
#
# `--self-test` proves the comparisons can still reject, without running the suite. That
# is not ceremony: the verdict here is four awk expressions, and an awk expression that
# stops comparing reports a clean pass forever.
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

# check_report applies both floors to a `go tool cover -func` report on stdin. Split out
# from the test run so the self-test can drive it with a synthetic report.
check_report() {
  report="$(cat)"
  status=0

  total="$(printf '%s\n' "$report" | awk '/^total:/ {gsub(/%/,"",$NF); print $NF}')"
  if [ -z "$total" ]; then
    echo "coverage: could not read a total out of the profile" >&2
    return 1
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

    # Match the function by name in its own file, so a same-named function elsewhere
    # cannot stand in for it — a silent substitution would report a pass for a function
    # nobody tested.
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

  return "$status"
}

# ── self-test ───────────────────────────────────────────────────────────────
#
# Each case is a synthetic report that MUST be rejected, plus one that must pass. A gate
# whose rejections are never exercised is indistinguishable from a gate that has stopped
# rejecting, and the second reports success.
self_test() {
  ok=0

  passing="$(
    for entry in $CRITICAL_FUNCS; do
      printf 'github.com/rackctl/rackctl/%s:1:\t%s\t100.0%%\n' "${entry%%:*}" "${entry##*:}"
    done
    printf 'total:\t\t\t(statements)\t%s.0%%\n' "$GLOBAL_FLOOR"
  )"

  expect_reject() {
    if printf '%s\n' "$2" | check_report >/dev/null 2>&1; then
      echo "self-test: $1 was ACCEPTED — this gate cannot reject" >&2
      ok=1
    else
      echo "self-test: $1 rejected"
    fi
  }

  if printf '%s\n' "$passing" | check_report >/dev/null 2>&1; then
    echo "self-test: a clean report passes"
  else
    echo "self-test: a clean report was rejected — the gate fails everything, which is the same as failing nothing" >&2
    ok=1
  fi

  expect_reject "a total below the floor" \
    "$(printf '%s\n' "$passing" | sed "s/(statements)\t${GLOBAL_FLOOR}.0%/(statements)\t1.0%/")"

  # awk rather than sed: BSD sed has no address 0, so a `0,/re/` range silently matches
  # nothing and the "mutated" report comes back identical — a self-test that proves the
  # gate accepts a report it was never actually asked about.
  expect_reject "a destructive-path function under 100%" \
    "$(printf '%s\n' "$passing" | awk '!done && /orphanedNodes/ { sub(/100\.0%/, "99.9%"); done=1 } { print }')"

  expect_reject "a destructive-path function missing from the profile" \
    "$(printf '%s\n' "$passing" | awk '!/orphanedNodes/')"

  expect_reject "a report with no total line" \
    "$(printf '%s\n' "$passing" | grep -v '^total:')"

  # A same-named function in another file must not satisfy the entry it is not.
  expect_reject "a same-named function standing in from the wrong file" \
    "$(printf '%s\n' "$passing" \
      | sed 's|rackctl/internal/reap/own.go:1:\tProves|rackctl/internal/elsewhere/other.go:1:\tProves|')"

  [ "$ok" -eq 0 ] && echo "self-test: coverage gate can reject"
  return "$ok"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

go test -coverprofile="$PROFILE" -covermode=set ./... >/dev/null
go tool cover -func="$PROFILE" | check_report
