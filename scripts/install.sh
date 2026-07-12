#!/bin/sh
# rackctl installer — curl -fsSL rackctl.com/install | sh
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
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')"
fi

if [ -z "$VERSION" ]; then
  echo "no release found — falling back to: go install github.com/${REPO}@latest" >&2
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
