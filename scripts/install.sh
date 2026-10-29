#!/bin/sh
# UPAX attendance relay installer for Linux and macOS.
#
#   curl -fsSL https://<host>/install.sh | sh
#
# Environment overrides:
#   UPAX_RELAY_BASE_URL  where release archives are downloaded from
#   UPAX_RELAY_SKIP_SETUP=1  only install the binary
set -eu

BASE_URL="${UPAX_RELAY_BASE_URL:-https://github.com/UPAX-platform/attendance-relay/releases/latest/download}"
BIN_DIR="/usr/local/bin"

say() { printf '%s\n' "$*"; }
die() { say "error: $*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  armv6l|armv7l|armhf) arch=arm ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "please run as root"
  SUDO="sudo"
fi

name="upax-relay_${os}_${arch}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  else
    wget -qO "$2" "$1"
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    die "need sha256sum or shasum to verify the download"
  fi
}

say "Downloading $name ..."
fetch "$BASE_URL/$name.tar.gz" "$tmp/relay.tgz"
fetch "$BASE_URL/checksums.txt" "$tmp/checksums.txt"
want="$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $name.tar.gz"
[ "$(sha256 "$tmp/relay.tgz")" = "$want" ] || die "checksum mismatch for $name.tar.gz; the download is corrupt or was tampered with"
tar -xzf "$tmp/relay.tgz" -C "$tmp"

$SUDO mkdir -p "$BIN_DIR"
$SUDO install -m 0755 "$tmp/$name/upax-relay" "$BIN_DIR/upax-relay"
say "Installed $BIN_DIR/upax-relay ($("$BIN_DIR/upax-relay" version))"

if [ "${UPAX_RELAY_SKIP_SETUP:-}" = "1" ]; then
  say "Run 'sudo upax-relay setup' to configure it."
  exit 0
fi

# If upgrading an existing install, just restart the service.
if $SUDO "$BIN_DIR/upax-relay" status 2>/dev/null | grep -q "service: running"; then
  $SUDO "$BIN_DIR/upax-relay" restart
  say "Relay upgraded and restarted."
  exit 0
fi

# stdin is the curl pipe; talk to the user's terminal instead.
if [ -r /dev/tty ]; then
  $SUDO "$BIN_DIR/upax-relay" setup </dev/tty
else
  say "Run 'sudo upax-relay setup' to configure it."
fi
