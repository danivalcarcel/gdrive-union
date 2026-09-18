#!/usr/bin/env bash
# Installs the latest (or a specific) gdunion release binary for this
# machine's OS/architecture.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/danivalcarcel/gdrive-union/master/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/danivalcarcel/gdrive-union/master/install.sh | bash -s v0.2.0
#
#   ./install.sh            # installs the latest release
#   ./install.sh v0.2.0     # installs a specific release tag
#
# Env vars:
#   GDUNION_INSTALL_DIR   where to put the binary (default: /usr/local/bin)

set -euo pipefail

REPO="danivalcarcel/gdrive-union"
VERSION="${1:-latest}"
INSTALL_DIR="${GDUNION_INSTALL_DIR:-/usr/local/bin}"

case "$(uname -s)" in
  Linux)  goos=linux ;;
  Darwin) goos=darwin ;;
  *)
    echo "gdunion only supports Linux and macOS (FUSE isn't available on this OS)." >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *)
    echo "Unsupported CPU architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

asset="gdunion-${goos}-${goarch}"

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset (release: $VERSION)..."
if ! curl -fsSL -o "$tmp/$asset" "$url"; then
  echo "Download failed: $url" >&2
  echo "Check that release '$VERSION' exists and has a '$asset' asset." >&2
  exit 1
fi

chmod +x "$tmp/$asset"

dest="$INSTALL_DIR/gdunion"
if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/$asset" "$dest"
else
  echo "Need elevated permissions to write to $INSTALL_DIR:"
  sudo mv "$tmp/$asset" "$dest"
fi

echo "Installed to $dest"
echo "Next: gdunion auth add <account-name>"
