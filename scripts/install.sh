#!/bin/sh
# rackctl installer — curl -fsSL rackctl.sh/install | sh
set -e

REPO="rackctl/rackctl"
BIN="rackctl"
INSTALL_DIR="${RACKCTL_INSTALL_DIR:-/usr/local/bin}"
VERSION="${RACKCTL_VERSION:-latest}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  arm64 | aarch64) ARCH="arm64" ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  # The HTTP status is read, not just the body. A repository with no releases answers 404,
  # and so does nothing else here — while a rate limit answers 403 and an outage answers 5xx.
  # `curl -f` collapses all of those into exit 22, and the pipeline that used to parse this
  # discarded even that: the status of `curl | grep | head | sed` is sed's, which always
  # succeeds. An empty result then reached a fallback that read it as "this project ships no
  # binaries" and installed from source WITHOUT the checksum verification below.
  #
  # Those are different worlds and the operator has to know which one they are in. A transient
  # failure is a REFUSAL — retrying gets them the verified binary. Only a genuine 404 is a
  # reason to build from source.
  rc=0
  body="$(curl -sSL -w '\n%{http_code}' "https://api.github.com/repos/${REPO}/releases/latest")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "could not reach the release API: curl exited $rc. This is a transient failure, not a" >&2
    echo "missing release — retry rather than installing unverified. To build from source" >&2
    echo "deliberately: go install github.com/${REPO}@latest" >&2
    exit 2
  fi
  code="$(printf '%s\n' "$body" | tail -1)"
  case "$code" in
    200) VERSION="$(printf '%s\n' "$body" | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')" ;;
    404) VERSION="" ;;
    *)   echo "the release API answered HTTP $code. That is a transient failure, not a missing" >&2
         echo "release — retry rather than installing unverified. To build from source" >&2
         echo "deliberately: go install github.com/${REPO}@latest" >&2
         exit 2 ;;
  esac
fi

if [ -z "$VERSION" ]; then
  # Reached only when the API ANSWERED and this repository genuinely publishes no release.
  echo "no release published — falling back to: go install github.com/${REPO}@latest" >&2
  exec go install "github.com/${REPO}@latest"
fi

URL="https://github.com/${REPO}/releases/download/${VERSION}/${BIN}_${OS}_${ARCH}.tar.gz"
SUMS="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading ${BIN} ${VERSION} (${OS}/${ARCH})..."
curl -fsSL "$URL" -o "$TMP/${BIN}.tar.gz"
curl -fsSL "$SUMS" -o "$TMP/checksums.txt"

echo "verifying checksum..."
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi
expected="$(grep " ${BIN}_${OS}_${ARCH}.tar.gz\$" "$TMP/checksums.txt" | awk '{print $1}')"
actual="$($SHA "$TMP/${BIN}.tar.gz" | awk '{print $1}')"
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "checksum verification failed" >&2
  exit 1
fi

tar -xzf "$TMP/${BIN}.tar.gz" -C "$TMP"

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP/$BIN" "$INSTALL_DIR/$BIN"
else
  sudo mv "$TMP/$BIN" "$INSTALL_DIR/$BIN"
fi
chmod +x "$INSTALL_DIR/$BIN"

echo "installed $("$INSTALL_DIR/$BIN" version)"
