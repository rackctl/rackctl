#!/bin/sh
# Runs scripts/install.sh end to end against a local fake release.
#
# `sh -n` is a SYNTAX check, not a compatibility check. dash -n accepts [[ ]], mapfile,
# declare -A and case-modification expansion; only executing the script rejects them. So the
# installer is EXECUTED here, under whichever shell is being tested, with curl stubbed and
# the install directory redirected. That also reaches the checksum path, which is the part
# of this script an operator's security depends on and which no parser can evaluate.
#
# Run as: sh scripts/install_test.sh   (the shell running this file is the shell under test)
set -e

SELF_SHELL="${TEST_SHELL:-/bin/sh}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "install_test: $1" >&2; exit 1; }

# A fake release: a tarball holding an executable that answers `version`.
mkdir -p "$WORK/src" "$WORK/serve" "$WORK/bin" "$WORK/stub"
printf '#!/bin/sh\necho v0.0.0-test\n' > "$WORK/src/rackctl"
chmod +x "$WORK/src/rackctl"
# Named into a variable rather than reached through a glob. A redirection target is NOT
# pathname-expanded by dash, though bash expands it — `>> dir/*.tar.gz` under dash creates a
# file called "*.tar.gz" and the tarball the installer reads is left untouched, so the
# tamper control would pass having tampered with nothing.
TARBALL="$WORK/serve/rackctl_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz"
tar -czf "$TARBALL" -C "$WORK/src" rackctl

if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi
( cd "$WORK/serve" && $SHA ./*.tar.gz | sed 's| \./| |' > checksums.txt )

# curl stub: -o NAME URL serves the basename out of $WORK/serve.
cat > "$WORK/stub/curl" <<'STUB'
#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
name="${url##*/}"
[ -f "$SERVE/$name" ] || exit 22
if [ -n "$out" ]; then cp "$SERVE/$name" "$out"; else cat "$SERVE/$name"; fi
STUB
chmod +x "$WORK/stub/curl"
export SERVE="$WORK/serve"

run_installer() {
  PATH="$WORK/stub:$PATH" RACKCTL_VERSION=v0.0.0-test RACKCTL_INSTALL_DIR="$WORK/bin" \
    "$SELF_SHELL" ./scripts/install.sh > "$WORK/out" 2>&1
}

# --- control 1: a good release installs, and the installed binary is the one served ---
rm -f "$WORK/bin/rackctl"
if ! run_installer; then cat "$WORK/out" >&2; fail "control 1: a valid release failed to install"; fi
[ -x "$WORK/bin/rackctl" ] || fail "control 1: nothing was installed"
grep -q "installed v0.0.0-test" "$WORK/out" || { cat "$WORK/out" >&2; fail "control 1: wrong version reported"; }
echo "install_test: a valid release installs and reports its version"

# --- control 2: a tarball that does not match checksums.txt is REJECTED ---
# The control is two-sided: control 1 above proved the same path ACCEPTS a good tarball, so
# a rejection here cannot be a script that rejects everything.
rm -f "$WORK/bin/rackctl"
cp "$TARBALL" "$WORK/good.tar.gz"
printf 'tampered' >> "$TARBALL"
# Sizes, not `cmp`. `cmp -s A B && fail ...` fails OPEN: with cmp absent the command exits
# 127, the && short-circuits, and the check that the tamper LANDED is silently skipped —
# which is the exact failure this line was added to catch, since a mutation that does not
# mutate makes control 2 pass having tampered with nothing. Compared this way an absent `wc`
# leaves both sides empty, the inequality is false, and the guard fires.
before="$(wc -c < "$WORK/good.tar.gz")"
after="$(wc -c < "$TARBALL")"
[ "$before" != "$after" ] || fail "control 2: the tamper did not change the tarball (before=$before after=$after)"
if run_installer; then cat "$WORK/out" >&2; fail "control 2: a tampered tarball INSTALLED"; fi
grep -q "checksum verification failed" "$WORK/out" || { cat "$WORK/out" >&2; fail "control 2: rejected without naming the checksum"; }
[ ! -e "$WORK/bin/rackctl" ] || fail "control 2: rejected but installed anyway"
cp "$WORK/good.tar.gz" "$TARBALL"
echo "install_test: a tampered tarball is rejected, naming the checksum, and installs nothing"

# --- control 3: an absent checksum line is REJECTED, not treated as a match ---
# An empty `expected` compared against a real `actual` must not pass. This is the branch
# where a missing entry silently becomes "no mismatch".
rm -f "$WORK/bin/rackctl"
: > "$WORK/serve/checksums.txt"
if run_installer; then cat "$WORK/out" >&2; fail "control 3: an empty checksums.txt INSTALLED"; fi
grep -q "checksum verification failed" "$WORK/out" || { cat "$WORK/out" >&2; fail "control 3: rejected for the wrong reason"; }
echo "install_test: an absent checksum line is rejected rather than read as a match"

echo "install_test: 3 controls passed under $SELF_SHELL"
