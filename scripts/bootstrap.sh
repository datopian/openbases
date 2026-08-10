#!/usr/bin/env bash
# Prepare a clean clone for development.
#
# Installs the exact gt/bd/dolt versions pinned in versions.lock into
# .toolchain/bin, verifying each artefact's SHA-256 before use. Nothing is
# installed system-wide and no version is resolved as "latest" (plan section 7.6).
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/lockfile.sh

TOOLCHAIN="${TOOLCHAIN_DIR:-.toolchain}"
BIN="$TOOLCHAIN/bin"
mkdir -p "$BIN"

PLAT="$(platform_key)"
echo "==> platform: $PLAT"

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}
require curl
require tar
require go

sha_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

install_tool() {
  local tool="$1" binname="$2"
  local version repo file want url tmp got

  version="$(lock_version "$tool")"
  repo="$(lock_repo "$tool")"
  file="$(lock_file "$tool" "$PLAT")"
  want="$(lock_sha "$tool" "$PLAT")"

  if [ -z "$version" ] || [ -z "$file" ] || [ -z "$want" ]; then
    echo "versions.lock has no entry for $tool on $PLAT" >&2
    exit 1
  fi

  # Already installed at the pinned version?
  if [ -x "$BIN/$binname" ] && "$BIN/$binname" --version 2>/dev/null | grep -qF "${version#v}"; then
    echo "==> $tool ${version} already installed"
    return 0
  fi

  url="https://github.com/${repo}/releases/download/${version}/${file}"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  echo "==> downloading $tool $version"
  curl -fsSL --retry 3 -o "$tmp/$file" "$url"

  got="$(sha_of "$tmp/$file")"
  if [ "$got" != "$want" ]; then
    echo "CHECKSUM MISMATCH for $file" >&2
    echo "  expected: $want" >&2
    echo "  actual:   $got" >&2
    echo "Refusing to install. Do not work around this." >&2
    exit 1
  fi
  echo "    checksum verified"

  tar -xzf "$tmp/$file" -C "$tmp"
  local found
  found="$(find "$tmp" -type f -name "$binname" -perm -u+x | head -1)"
  if [ -z "$found" ]; then
    echo "could not find '$binname' inside $file" >&2
    exit 1
  fi
  install -m 0755 "$found" "$BIN/$binname"
  echo "    installed $BIN/$binname"
}

echo "==> installing pinned toolchain"
install_tool dolt    dolt
install_tool beads   bd
install_tool gastown gt

echo "==> Go modules"
go mod download

if [ -f apps/web/package-lock.json ]; then
  echo "==> web dependencies"
  ( cd apps/web && npm ci --no-audit --no-fund )
else
  echo "==> web dependencies skipped (no lockfile yet)"
fi

echo
echo "Bootstrap complete. Add the pinned toolchain to your PATH for this repo:"
echo "  export PATH=\"\$PWD/$BIN:\$PATH\""
echo
echo "Then: make check"
