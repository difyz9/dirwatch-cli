#!/usr/bin/env bash
# Install dmon (Directory Monitor CLI) from the latest GitHub release.
#
# Detects the OS and architecture, downloads the matching release asset,
# verifies its sha256 checksum against checksums.txt, and installs the
# binary into ~/.local/bin (override the destination with DMON_INSTALL_DIR).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/difyz9/dmon-cli/main/scripts/install.sh | bash
set -euo pipefail

REPO="difyz9/dmon-cli"
BIN="dmon"
INSTALL_DIR="${DMON_INSTALL_DIR:-${XDG_BIN_HOME:-$HOME/.local/bin}}"

# --- Detect OS ---
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux | darwin) ;;
  *) echo "Unsupported OS '$OS' (on Windows, download the .zip from the release page)." >&2; exit 1 ;;
esac

# --- Detect architecture ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture '$ARCH'." >&2; exit 1 ;;
esac

# --- Resolve the latest release tag ---
echo "==> Latest release for $OS/$ARCH"
TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep -m1 '"tag_name"' | cut -d '"' -f4)"
if [ -z "$TAG" ]; then
  echo "Could not determine the latest release tag." >&2
  exit 1
fi
echo "    $TAG"

ASSET="dmon_${TAG}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$TAG"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- Download the asset and its checksums ---
echo "==> Downloading $ASSET"
curl -fsSL "$BASE/$ASSET" -o "$tmp/$ASSET"
curl -fsSL "$BASE/checksums.txt" -o "$tmp/checksums.txt"

# --- Verify the sha256 checksum ---
echo "==> Verifying sha256 checksum"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$ASSET" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp/$ASSET" | awk '{print $1}')"
fi
expected="$(grep " $ASSET\$" "$tmp/checksums.txt" | awk '{print $1}')"
if [ -z "$expected" ]; then
  echo "    $ASSET not listed in checksums.txt; skipping verification" >&2
elif [ "$expected" != "$actual" ]; then
  echo "Checksum mismatch for $ASSET (expected $expected, got $actual)." >&2
  exit 1
fi

# --- Extract and install ---
echo "==> Installing to $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
tar -xzf "$tmp/$ASSET" -C "$tmp"
install -m 0755 "$tmp/$BIN" "$INSTALL_DIR/$BIN"

# macOS: drop the Gatekeeper quarantine flag if present (best effort).
if [ "$OS" = "darwin" ]; then
  xattr -dr com.apple.quarantine "$INSTALL_DIR/$BIN" 2>/dev/null || true
fi

if ! printf '%s' "$PATH" | tr ':' '\n' | grep -Fxq "$INSTALL_DIR"; then
  echo "    Note: $INSTALL_DIR is not on PATH. Add it with:" >&2
  echo "      export PATH=\"$INSTALL_DIR:\$PATH\"" >&2
fi

"$INSTALL_DIR/$BIN" --version
echo "dmon installed to $INSTALL_DIR/$BIN"
