#!/usr/bin/env bash
# Installs the latest (or a specific) gdunion release binary for this
# machine's OS/architecture, and optionally sets it up as a systemd --user
# service so the mount comes back on its own instead of running `gdunion
# mount` by hand every time.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/danivalcarcel/gdrive-union/master/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/danivalcarcel/gdrive-union/master/install.sh | bash -s -- --service
#
#   ./install.sh                # installs the latest release
#   ./install.sh v0.2.0         # installs a specific release tag
#   ./install.sh --service      # installs latest, and sets up+starts the systemd service
#   ./install.sh v0.2.0 --service
#
# Env vars:
#   GDUNION_INSTALL_DIR   where to put the binary (default: /usr/local/bin)
#   GDUNION_MOUNTPOINT    where the service mounts to (default: $HOME/gdrive)

set -euo pipefail

REPO="danivalcarcel/gdrive-union"
INSTALL_DIR="${GDUNION_INSTALL_DIR:-/usr/local/bin}"

VERSION="latest"
SETUP_SERVICE=0
for arg in "$@"; do
  case "$arg" in
    --service) SETUP_SERVICE=1 ;;
    *) VERSION="$arg" ;;
  esac
done

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

setup_service() {
  if [ "$goos" != "linux" ]; then
    echo "The --service setup is systemd-specific (Linux only); skipping it on $goos." >&2
    echo "Next: gdunion auth add <account-name>"
    return
  fi
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found - skipping --service setup. Is this a systemd-based distro?" >&2
    echo "Next: gdunion auth add <account-name>"
    return
  fi

  local mountpoint="${GDUNION_MOUNTPOINT:-$HOME/gdrive}"
  local unit_dir="$HOME/.config/systemd/user"
  local unit_path="$unit_dir/gdunion.service"

  mkdir -p "$unit_dir"
  cat > "$unit_path" <<EOF
[Unit]
Description=gdunion - Google Drive union filesystem mount
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$dest mount $mountpoint
Restart=on-failure
RestartSec=10
StartLimitIntervalSec=120
StartLimitBurst=5

[Install]
WantedBy=default.target
EOF
  echo "Wrote $unit_path"

  systemctl --user daemon-reload
  systemctl --user enable gdunion.service
  systemctl --user restart gdunion.service

  echo "Service enabled and (re)started, mounting at $mountpoint."

  if loginctl enable-linger "$(id -un)" >/dev/null 2>&1; then
    echo "Linger enabled: it'll come back after a reboot even without logging in."
  else
    echo "Could not enable linger automatically - without it, the service only runs while you have an active login session." >&2
    echo "Try: sudo loginctl enable-linger $(id -un)" >&2
  fi

  echo
  echo "If you haven't yet: gdunion auth add <account-name>, then:"
  echo "  systemctl --user restart gdunion.service"
  echo "Status: systemctl --user status gdunion.service"
  echo "Logs:   journalctl --user -u gdunion.service -f"
}

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
  checksums_url="https://github.com/${REPO}/releases/latest/download/SHA256SUMS"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  checksums_url="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS"
fi

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "Need sha256sum or shasum to verify the download; neither is on PATH." >&2
    exit 1
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset (release: $VERSION)..."
if ! curl -fsSL -o "$tmp/$asset" "$url"; then
  echo "Download failed: $url" >&2
  echo "Check that release '$VERSION' exists and has a '$asset' asset." >&2
  exit 1
fi

echo "Verifying checksum..."
if ! curl -fsSL -o "$tmp/SHA256SUMS" "$checksums_url"; then
  echo "Could not download checksums: $checksums_url" >&2
  echo "Refusing to install an unverified binary. If '$VERSION' predates" >&2
  echo "published checksums, install a newer release instead." >&2
  exit 1
fi

expected="$(awk -v asset="$asset" '$2 == asset { print $1 }' "$tmp/SHA256SUMS")"
if [ -z "$expected" ]; then
  echo "No checksum for '$asset' found in SHA256SUMS - refusing to install." >&2
  exit 1
fi

actual="$(sha256_of "$tmp/$asset")"
if [ "$actual" != "$expected" ]; then
  echo "Checksum mismatch for $asset - the download may be corrupted or tampered with." >&2
  echo "Expected: $expected" >&2
  echo "Got:      $actual" >&2
  exit 1
fi
echo "Checksum OK."

chmod +x "$tmp/$asset"

dest="$INSTALL_DIR/gdunion"
if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/$asset" "$dest"
else
  echo "Need elevated permissions to write to $INSTALL_DIR:"
  sudo mv "$tmp/$asset" "$dest"
fi

echo "Installed to $dest"

if [ "$SETUP_SERVICE" -eq 1 ]; then
  setup_service
else
  echo "Next: gdunion auth add <account-name>"
  echo "(or re-run this script with --service to also set up a systemd user service that keeps it mounted)"
fi
