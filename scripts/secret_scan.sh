#!/usr/bin/env bash
# Scan the repository history for committed secrets.
#
# The gitleaks GitHub Action requires a paid licence for organisation-owned
# repositories. The gitleaks binary is MIT-licensed and free, so CI installs it
# directly — pinned by version and verified by checksum, on the same principle
# as versions.lock.
set -euo pipefail

GITLEAKS_VERSION="${GITLEAKS_VERSION:-8.30.1}"

# Both architectures, because the runner may be either.
#
# This used to hardcode linux_x64, which failed on an arm64 CI runner with
# "cannot execute binary file: Exec format error" — a message that reads like a
# corrupt download rather than the wrong platform. gitleaks publishes an arm64
# build, so the fix is selecting one, not dropping the check.
case "$(uname -m)" in
  x86_64|amd64)
    GITLEAKS_ARCH="x64"
    GITLEAKS_SHA256="${GITLEAKS_SHA256:-551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb}"
    ;;
  aarch64|arm64)
    GITLEAKS_ARCH="arm64"
    GITLEAKS_SHA256="${GITLEAKS_SHA256:-e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080}"
    ;;
  *)
    echo "no pinned gitleaks build for $(uname -m); add its checksum rather than skipping the scan" >&2
    exit 1
    ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

file="gitleaks_${GITLEAKS_VERSION}_linux_${GITLEAKS_ARCH}.tar.gz"
url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/${file}"

echo "==> installing gitleaks ${GITLEAKS_VERSION}"
curl -fsSL --retry 3 -o "$tmp/$file" "$url"

got="$(sha256sum "$tmp/$file" | awk '{print $1}')"
if [ "$got" != "$GITLEAKS_SHA256" ]; then
  echo "CHECKSUM MISMATCH for $file" >&2
  echo "  expected: $GITLEAKS_SHA256" >&2
  echo "  actual:   $got" >&2
  exit 1
fi

tar -xzf "$tmp/$file" -C "$tmp"
chmod +x "$tmp/gitleaks"

echo "==> scanning working tree"
"$tmp/gitleaks" dir . --no-banner --redact --exit-code 1

echo "==> scanning git history"
"$tmp/gitleaks" git . --no-banner --redact --exit-code 1

echo "no secrets detected"
