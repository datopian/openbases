#!/usr/bin/env bash
# Scan the repository history for committed secrets.
#
# The gitleaks GitHub Action requires a paid licence for organisation-owned
# repositories. The gitleaks binary is MIT-licensed and free, so CI installs it
# directly — pinned by version and verified by checksum, on the same principle
# as versions.lock.
set -euo pipefail

GITLEAKS_VERSION="${GITLEAKS_VERSION:-8.30.1}"

# Every platform this is actually run on, because a scan that cannot run is a
# scan that does not happen.
#
# This used to hardcode linux_x64, which failed on an arm64 CI runner with
# "cannot execute binary file: Exec format error" — a message that reads like a
# corrupt download rather than the wrong platform. It was then fixed for arm64
# and still hardcoded `linux`, so it failed the same way, with the same
# misleading message, on a macOS laptop. Both times the effect was that the
# secret scan silently stopped being run outside CI.
#
# Checksums are from the release's own checksums.txt, pinned here rather than
# fetched, because fetching the checksum from the same place as the artefact
# verifies nothing.
case "$(uname -s)" in
  Linux)  GITLEAKS_OS="linux" ;;
  Darwin) GITLEAKS_OS="darwin" ;;
  *)
    echo "no pinned gitleaks build for $(uname -s); add its checksum rather than skipping the scan" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) GITLEAKS_ARCH="x64" ;;
  aarch64|arm64) GITLEAKS_ARCH="arm64" ;;
  *)
    echo "no pinned gitleaks build for $(uname -m); add its checksum rather than skipping the scan" >&2
    exit 1
    ;;
esac

case "${GITLEAKS_OS}_${GITLEAKS_ARCH}" in
  linux_x64)   GITLEAKS_SHA256_DEFAULT="551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb" ;;
  linux_arm64) GITLEAKS_SHA256_DEFAULT="e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080" ;;
  darwin_x64)  GITLEAKS_SHA256_DEFAULT="dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709" ;;
  darwin_arm64) GITLEAKS_SHA256_DEFAULT="b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5" ;;
esac
GITLEAKS_SHA256="${GITLEAKS_SHA256:-$GITLEAKS_SHA256_DEFAULT}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

file="gitleaks_${GITLEAKS_VERSION}_${GITLEAKS_OS}_${GITLEAKS_ARCH}.tar.gz"
url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/${file}"

echo "==> installing gitleaks ${GITLEAKS_VERSION}"
curl -fsSL --retry 3 -o "$tmp/$file" "$url"

if command -v sha256sum >/dev/null; then
  got="$(sha256sum "$tmp/$file" | awk '{print $1}')"
else
  # macOS ships shasum, not sha256sum.
  got="$(shasum -a 256 "$tmp/$file" | awk '{print $1}')"
fi
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

# The history pass is bounded to the commits actually under test.
#
# Without --log-opts, gitleaks scans EVERY ref in the clone. On the self-hosted
# runner that clone is persistent and reused across jobs, and every pull request
# ever built leaves a refs/remotes/pull/N/merge behind — actions/checkout's
# --prune only covers refs/heads, so those accumulate forever. A secret in any
# commit ever checked out there then fails this job on every later pull request,
# after the branch is deleted and the pull request closed, even though the
# commit is not in the repository any more (wg-cjp).
#
# Measured on the same commit: a fresh clone with every branch fetched scanned
# 113 commits and found nothing; the runner scanned 164 and found one, in a
# commit a fresh clone cannot see.
#
# HEAD is the right bound and not a weaker one. On a pull request HEAD is the
# merge ref, so its ancestry is the proposed change plus the base branch. On a
# push to main HEAD is main, so main's full history is still scanned. What stops
# being scanned is refs that are not in the repository — which was never a scope
# anyone chose, it was whatever the runner had lying around.
echo "==> scanning git history"
"$tmp/gitleaks" git . --no-banner --redact --exit-code 1 --log-opts=HEAD

echo "no secrets detected"
