#!/usr/bin/env bash
# Minimal reader for versions.lock. The lock file is small, flat, and stable, so
# a dependency-free reader is preferable to requiring yq on every machine.
set -uo pipefail

LOCKFILE="${LOCKFILE:-versions.lock}"

# lock_version <tool>  ->  e.g. v1.2.0
lock_version() {
  awk -v tool="$1" '
    $0 ~ "^  " tool ":$" { in_tool = 1; next }
    in_tool && /^  [a-z]/ && $0 !~ "^  " tool ":$" { in_tool = 0 }
    in_tool && /^    version:/ {
      gsub(/^ *version: *"?/, ""); gsub(/"$/, ""); print; exit
    }
  ' "$LOCKFILE"
}

# lock_sha <tool> <platform>  ->  sha256 of that artefact
lock_sha() {
  awk -v tool="$1" -v plat="$2" '
    $0 ~ "^  " tool ":$" { in_tool = 1; next }
    in_tool && /^  [a-z]/ { in_tool = 0 }
    in_tool && $0 ~ "^      " plat ":$" { in_plat = 1; next }
    in_plat && /^      [a-z]/ { in_plat = 0 }
    in_plat && /^        sha256:/ {
      gsub(/^ *sha256: *"?/, ""); gsub(/"$/, ""); print; exit
    }
  ' "$LOCKFILE"
}

# lock_file <tool> <platform>  ->  release artefact filename
lock_file() {
  awk -v tool="$1" -v plat="$2" '
    $0 ~ "^  " tool ":$" { in_tool = 1; next }
    in_tool && /^  [a-z]/ { in_tool = 0 }
    in_tool && $0 ~ "^      " plat ":$" { in_plat = 1; next }
    in_plat && /^      [a-z]/ { in_plat = 0 }
    in_plat && /^        file:/ {
      gsub(/^ *file: *"?/, ""); gsub(/"$/, ""); print; exit
    }
  ' "$LOCKFILE"
}

# lock_repo <tool>
lock_repo() {
  awk -v tool="$1" '
    $0 ~ "^  " tool ":$" { in_tool = 1; next }
    in_tool && /^  [a-z]/ { in_tool = 0 }
    in_tool && /^    repository:/ {
      gsub(/^ *repository: *"?/, ""); gsub(/"$/, ""); print; exit
    }
  ' "$LOCKFILE"
}

# platform_key -> linux_amd64 | linux_arm64 | darwin_amd64 | darwin_arm64
platform_key() {
  local os arch
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) echo "unsupported OS: $(uname -s)" >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; return 1 ;;
  esac
  echo "${os}_${arch}"
}
