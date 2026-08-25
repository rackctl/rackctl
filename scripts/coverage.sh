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
# The positive controls run on EVERY invocation, ahead of the real check — not behind a
# flag. There is then no CI step to forget and no flag a caller can silently omit, so the
# gate cannot drift from the workflow that calls it. That is not ceremony: the verdict here
# is four awk expressions, and an awk expression that stops comparing reports a clean pass
# forever.
#
# The controls here mutate a GENERATED report with sed and awk rather than constructing
# each fixture from a literal the way scripts/pins.py does. That is a real difference in
# assurance: a patch can apply and change nothing that matters, so this file carries the
# landed/absent-before/present-after assertions that pins.py does not need.
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

  checked=0
  for entry in $CRITICAL_FUNCS; do
    checked=$((checked + 1))
    file="${entry%%:*}"
    func="${entry##*:}"

    # Match the function by name in its own file, so a same-named function elsewhere
    # cannot stand in for it — a silent substitution would report a pass for a function
    # nobody tested.
    #
    # EVERY match, not the first. A search that takes the first hit and stops cannot tell
    # one match from several, and if two ever collide the one it happens to reach first
    # decides the verdict — which is how a dead copy above a live one wins a search. More
    # than one is itself the finding: the entry no longer names exactly one function.
    matches="$(printf '%s\n' "$report" \
      | awk -v f="/$file:" -v n="$func" '$0 ~ f && $(NF-1) == n { gsub(/%/,"",$NF); print $NF }')"
    count="$(printf '%s' "$matches" | grep -c . || true)"
    if [ "$count" -gt 1 ]; then
      echo "coverage: $file:$func matches $count entries in the profile ($(printf '%s' "$matches" | tr '\n' ' ')) — the entry no longer names exactly one function, so which one is measured is arbitrary" >&2
      status=1
      continue
    fi
    got="$matches"

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

  # The denominator. A pass over zero critical functions and a pass over all of them read
  # identically without it, and the first means the list emptied rather than the tree
  # improved.
  echo "coverage: $checked destructive-path function(s) checked"
  return "$status"
}

# ── positive controls ───────────────────────────────────────────────────────
#
# Each control introduces the exact violation a check exists to catch, and the gate must
# reject it. The clean fixture is asserted to PASS first: without that, a rejection proves
# nothing, because the gate might have been failing for an unrelated reason all along.
#
# A gate whose rejections are never exercised is indistinguishable from one that has
# stopped rejecting, and the second reports success.
self_test() {
  ok=0
  [ -n "$CRITICAL_FUNCS" ] || {
    echo "self-test: the destructive-path list is empty — this gate would pass anything" >&2
    return 1
  }

  passing="$(
    for entry in $CRITICAL_FUNCS; do
      printf 'github.com/rackctl/rackctl/%s:1:\t%s\t100.0%%\n' "${entry%%:*}" "${entry##*:}"
    done
    printf 'total:\t\t\t(statements)\t%s.0%%\n' "$GLOBAL_FLOOR"
  )"

  # A mutation counts as landed only when the text CHANGED, the marker it claimed to plant
  # is present AFTER, and was absent BEFORE. An edit can apply cleanly and change nothing
  # that matters — a floor the fixture already meets, a pattern that matched nothing — and
  # the verdict alone records that as proof.
  #
  # Markers are SYNTHETIC tokens that occur nowhere but in a mutation. A realistic-looking
  # one may already be present, and then "present after" proves nothing: the surest place
  # to find realistic syntax is the documentation of the gates that catch it.
  #
  # $1 name, $2 mutated report, $3 the marker the mutation claims to plant.
  expect_reject() {
    if [ "$2" = "$passing" ]; then
      echo "control: $1 did not change the fixture — the mutation did not land, so its rejection would prove nothing" >&2
      ok=1
      return
    fi
    if [ -n "$3" ]; then
      if ! printf '%s\n' "$2" | grep -q -- "$3"; then
        echo "control: $1 claims to plant $3 and the mutated fixture does not contain it" >&2
        ok=1
        return
      fi
      if printf '%s\n' "$passing" | grep -q -- "$3"; then
        echo "control: $1 plants $3, which the CLEAN fixture already contains — the mutation changes nothing about the meaning" >&2
        ok=1
        return
      fi
    fi
    if printf '%s\n' "$2" | check_report >/dev/null 2>&1; then
      echo "control: $1 was ACCEPTED — this gate cannot reject" >&2
      ok=1
    else
      echo "control: $1 rejected"
    fi
  }

  if printf '%s\n' "$passing" | check_report >/dev/null 2>&1; then
    echo "control: a clean report passes"
  else
    echo "control: a clean report was rejected — the gate fails everything, which is the same as failing nothing" >&2
    ok=1
  fi

  expect_reject "a total below the floor" \
    "$(printf '%s\n' "$passing" | sed "s/(statements)\t${GLOBAL_FLOOR}.0%/(statements)\t13.37%/")" \
    "13.37%"

  # awk rather than sed: BSD sed has no address 0, so a `0,/re/` range silently matches
  # nothing and the "mutated" report comes back identical — a self-test that proves the
  # gate accepts a report it was never actually asked about.
  expect_reject "a destructive-path function under 100%" \
    "$(printf '%s\n' "$passing" | awk '!done && /orphanedNodes/ { sub(/100\.0%/, "42.24%"); done=1 } { print }')" \
    "42.24%"

  expect_reject "a destructive-path function missing from the profile" \
    "$(printf '%s\n' "$passing" | awk '!/orphanedNodes/')"

  expect_reject "a report with no total line" \
    "$(printf '%s\n' "$passing" | grep -v '^total:')"

  # Two entries matching one list item: whichever a first-hit search reached would decide
  # the verdict, and a dead copy above a live one is exactly how that goes wrong.
  expect_reject "an entry matching more than one function" \
    "$(printf '%s\n' "$passing" \
      | awk '$(NF-1) == "Proves" && !done { print; sub(/100\.0%/, "31.41%"); done=1 } { print }')" \
    "31.41%"

  # A same-named function in another file must not satisfy the entry it is not.
  expect_reject "a same-named function standing in from the wrong file" \
    "$(printf '%s\n' "$passing" \
      | sed 's|rackctl/internal/reap/own.go:1:\tProves|rackctl/internal/rackctl-ctl-elsewhere/other.go:1:\tProves|')" \
    "rackctl-ctl-elsewhere"

  [ "$ok" -eq 0 ] && echo "control: coverage gate can reject"
  return "$ok"
}

# --check-report <file> applies the floors to a `go tool cover -func` report the CALLER
# supplies, without running the suite. The report, not the raw profile: it is exactly the
# input the verdict logic reads, so the floor supplies the thing being judged rather than
# something upstream of it. It exists so scripts/floor.py can feed this gate a known-bad input and
# observe the exit status, rather than reading what the gate says about itself. A gate that
# reports its own verdict is testimony; a gate handed a bad input and watched to exit
# non-zero is evidence.
if [ "${1:-}" = "--check-report" ]; then
  [ -n "${2:-}" ] || { echo "coverage: --check-report needs a file" >&2; exit 2; }
  check_report < "$2"
  exit $?
fi

# Controls first, always.
self_test || exit 1
[ "${1:-}" = "--controls-only" ] && exit 0

go test -coverprofile="$PROFILE" -covermode=set ./... >/dev/null

# Not piped into check_report. A pipeline's status is its LAST element, so a failing
# `go tool cover` would hand check_report an empty report and vanish. Read directly rather
# than through `set -o pipefail`, which is a late addition to POSIX and to dash: the status
# of the command that matters should not depend on which /bin/sh is running the script.
REPORT="${TMPDIR:-/tmp}/rackctl-cover-func.$$"
trap 'rm -f "$REPORT"' EXIT
if ! go tool cover -func="$PROFILE" > "$REPORT"; then
  echo "coverage: go tool cover could not read $PROFILE — no verdict was reached" >&2
  exit 2
fi
check_report < "$REPORT"
