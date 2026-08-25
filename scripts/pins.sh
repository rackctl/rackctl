#!/bin/sh
# Assert that every version this repo pins is watched by Renovate.
#
# The description of what is covered is the thing that goes stale. A comment saying "the
# workflows carry no other pins" is true when written, silently false at the first edit
# that adds one, and there is no signal either way. So this asserts instead: it enumerates
# the pins in the tree and fails on any shape no manager in .github/renovate.json claims.
#
# The failure it exists for is the quiet one. An unwatched pin does not break — it works
# exactly as well as it did the day it was written, for as long as nobody looks, which on a
# security scanner means it keeps passing while its newer rules go unrun.
#
# `--self-test` proves the gate can still reject. Roots are overridable so the self-test
# can point it at fixtures.
set -e

RENOVATE="${RENOVATE:-.github/renovate.json}"
WORKFLOWS="${WORKFLOWS:-.github/workflows}"
GOMOD="${GOMOD:-go.mod}"

fail=0
found=0

note() { found=$((found + 1)); printf '  %-56s %s\n' "$1" "$2"; }
reject() { echo "pins: $1" >&2; fail=1; }

run_checks() {
  [ -f "$RENOVATE" ] || { echo "pins: $RENOVATE is missing — nothing watches any pin" >&2; return 1; }

  # ── 1. Action references ──────────────────────────────────────────────────
  #
  # Renovate's github-actions manager reads these natively, but only usefully when the pin
  # is a SHA carrying the version comment it rewrites alongside. A bare tag is mutable —
  # whoever can move it changes what runs — and a bare SHA tells Renovate nothing about
  # what version it currently is, so both are rejected.
  for wf in "$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml; do
    [ -f "$wf" ] || continue
    n=0
    # A here-string would fork a subshell and lose every `fail` set inside the loop, which
    # is the shape that makes a gate report success while finding problems.
    while IFS= read -r ref; do
      n=$((n + 1))
      case "$ref" in
        *@[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*)
          case "$ref" in
            *"# v"*) ;;
            *) reject "$wf: $ref — SHA pin with no version comment, so Renovate cannot tell what version it is" ;;
          esac ;;
        *@*) reject "$wf: $ref — pinned to a mutable tag rather than a commit SHA" ;;
      esac
    done <<EOF
$(grep -E '^[[:space:]]*-?[[:space:]]*uses:' "$wf" 2>/dev/null | sed -E 's/.*uses:[[:space:]]*//')
EOF
    [ "$n" -gt 0 ] && note "$wf" "$n action ref(s), github-actions manager"
  done

  # ── 2. Versions no native manager reads ───────────────────────────────────
  #
  # Each shape below is an input to an action, or a module named inside a shell command.
  # Renovate sees neither without a customManager, so each must be claimed by one. The
  # token grepped for is unique to that manager's matchStrings, so deleting or renaming a
  # manager fails here rather than going quiet.
  check_shape() {
    label="$1" pattern="$2" token="$3"
    hits="$(grep -rEl "$pattern" "$WORKFLOWS" 2>/dev/null || true)"
    [ -n "$hits" ] || return 0
    if grep -q "$token" "$RENOVATE"; then
      note "$label" "customManager present"
    else
      reject "$label is pinned in $(echo "$hits" | tr '\n' ' ')but no customManager in $RENOVATE claims it"
    fi
  }

  check_shape "go-version (setup-go)" 'go-version:[[:space:]]*"[0-9]+\.[0-9]+'      'golang-version'
  check_shape "golangci-lint version" 'version:[[:space:]]*v[0-9]+\.[0-9]+\.[0-9]+' 'golangci/golangci-lint'
  check_shape "govulncheck"           'govulncheck@v[0-9]+'                          'golang.org/x/vuln'
  check_shape "zizmor"                'zizmor==[0-9]+'                               'pypi'
  check_shape "goreleaser version"    'version:[[:space:]]*"~>[[:space:]]*v[0-9]+"' 'goreleaser/goreleaser'

  # ── 3. The module graph ───────────────────────────────────────────────────
  if [ -f "$GOMOD" ]; then
    if grep -q '"gomodTidy"' "$RENOVATE"; then
      note "$GOMOD" "$(grep -cE '^[[:space:]]+[a-z].*/.* v[0-9]' "$GOMOD" || echo 0) module(s), gomod manager"
    else
      reject "$GOMOD exists but $RENOVATE does not configure the gomod manager"
    fi
  fi

  if [ "$fail" -eq 0 ]; then
    echo "pins: $found pin group(s), all watched"
  fi
  return "$fail"
}

# ── self-test ─────────────────────────────────────────────────────────────────
#
# Each fixture is a tree this gate MUST reject, plus one it must accept. A gate whose
# rejections are never exercised is indistinguishable from one that has stopped rejecting,
# and the second reports success.
self_test() {
  ok=0
  root="$(mktemp -d)"
  trap 'rm -rf "$root"' EXIT

  build() {
    rm -rf "$root/wf"; mkdir -p "$root/wf"
    printf '%s\n' "$2" > "$root/wf/ci.yml"
    cp "${RENOVATE_SRC:-.github/renovate.json}" "$root/renovate.json"
    [ -n "$3" ] && printf '%s\n' "$3" > "$root/renovate.json"
    printf 'module x\n\ngo 1.26\n' > "$root/go.mod"
  }

  attempt() {
    ( RENOVATE="$root/renovate.json" WORKFLOWS="$root/wf" GOMOD="$root/go.mod" \
      sh "$0" >/dev/null 2>&1 )
  }

  good='jobs:
  build:
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4'

  build "" "$good" ""
  if attempt; then
    echo "self-test: a clean tree passes"
  else
    echo "self-test: a clean tree was REJECTED — a gate that fails everything fails nothing" >&2
    ok=1
  fi

  expect_reject() {
    build "" "$2" "$3"
    if attempt; then
      echo "self-test: $1 was ACCEPTED — this gate cannot reject" >&2
      ok=1
    else
      echo "self-test: $1 rejected"
    fi
  }

  expect_reject "an action on a mutable tag" 'jobs:
  build:
    steps:
      - uses: actions/checkout@v4' ""

  expect_reject "a SHA pin with no version comment" 'jobs:
  build:
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262' ""

  expect_reject "a go-version with no customManager claiming it" 'jobs:
  build:
    steps:
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5
        with:
          go-version: "1.26"' '{"extends":["config:recommended"],"postUpdateOptions":["gomodTidy"]}'

  expect_reject "a govulncheck pin with no customManager claiming it" 'jobs:
  build:
    steps:
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...' '{"extends":["config:recommended"],"postUpdateOptions":["gomodTidy"]}'

  expect_reject "a go.mod the gomod manager is not configured for" "$good" '{"extends":["config:recommended"]}'

  [ "$ok" -eq 0 ] && echo "self-test: pins gate can reject"
  return "$ok"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

run_checks
exit $?
