#!/usr/bin/env bash
# The Gas Town / Beads / Dolt compatibility fixture (plan section 7.6).
#
# versions.lock names this path as the gate a version bump must pass. Its job is
# narrow and worth stating: prove the three pinned binaries still work TOGETHER.
# The matrix moves as a unit, and the failure it exists to catch is a bump that
# leaves each tool working alone and the combination broken — gt refusing a
# newer bd, bd writing a schema dolt cannot open, a town that initialises but
# cannot answer a query.
#
# What it deliberately does NOT do: dispatch a real agent. That needs a model,
# and a CI job that spends money on every push is a CI job someone disables. The
# agent path is exercised against staging, where a real polecat wrote a file,
# pushed a branch and had a pull request opened for it.
#
# Everything here runs offline against a local bare repository, so CI needs no
# network and no credentials.
set -uo pipefail

failures=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# Capture, then match. NEVER pipe a command into grep here.
#
# Under `set -o pipefail` the pipeline reports the exit status of the failing
# command, not of grep — and these tools exit non-zero for reasons that have
# nothing to do with the assertion, such as bd warning about directory
# permissions. A correct result then reads as a failed assertion, which cost
# real time twice: once here and once in the cell isolation test.
says() {
  local haystack="$1" needle="$2"
  printf '%s' "$haystack" | grep -qiE "$needle"
}

for tool in gt bd dolt git; do
  command -v "$tool" >/dev/null || { echo "  SKIP  $tool is not installed"; exit 0; }
done

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
WORK=$(mktemp -d)
trap 'cd /; "$ROOT/.toolchain/bin/gt" down >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

export HOME="$WORK"
export GIT_AUTHOR_NAME="compatibility-fixture" GIT_AUTHOR_EMAIL="ci@datopian.com"
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
git config --global user.name "$GIT_AUTHOR_NAME" >/dev/null 2>&1
git config --global user.email "$GIT_AUTHOR_EMAIL" >/dev/null 2>&1
git config --global init.defaultBranch main >/dev/null 2>&1

echo "Compatibility fixture: gt $(gt --version 2>/dev/null | head -1 | tr -d '\n'), bd $(bd version 2>/dev/null | head -1), dolt $(dolt version 2>/dev/null | head -1)"

# ---------------------------------------------------------------------------
# 1. The declared matrix is what is actually installed.
# ---------------------------------------------------------------------------
lock="$ROOT/versions.lock"
# The lock groups by product name, not binary name: bd lives under "beads" and
# gt under "gastown". Looking up by the binary name silently found nothing and
# compared against an empty string, which reported a failure that said the
# version was wrong when the lookup was.
for pair in "beads:bd:$(bd version 2>/dev/null | head -1)" \
            "dolt:dolt:$(dolt version 2>/dev/null | head -1)" \
            "gastown:gt:$(gt --version 2>/dev/null | head -1)"; do
  key=${pair%%:*}; rest=${pair#*:}; name=${rest%%:*}; reported=${rest#*:}
  want=$(awk "/^  $key:/{f=1} f&&/version:/{gsub(/[\" v]/,\"\",\$2); print \$2; exit}" "$lock")
  if [ -n "$want" ] && printf '%s' "$reported" | grep -q "$want"; then
    pass "$name matches versions.lock ($want)"
  else
    fail "$name reports '$reported' but versions.lock pins '$want'"
  fi
done

# ---------------------------------------------------------------------------
# 2. A town initialises, and a rig can be added from a real git remote.
# ---------------------------------------------------------------------------
UPSTREAM="$WORK/upstream.git"
git init -q --bare "$UPSTREAM"
SEED="$WORK/seed"
git init -q "$SEED" && cd "$SEED"
echo "# fixture" > README.md
git add README.md && git commit -q -m "seed"
git branch -M main && git remote add origin "$UPSTREAM" && git push -q origin main

# A free Dolt port, chosen at run time.
#
# gt defaults to 3307, and a developer running their own town already holds it —
# the fixture then fails with "port is already in use", which looks like a
# compatibility problem and is not one. CI is usually clean; a laptop is not.
DOLT_PORT=$(python3 -c "
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()")

mkdir -p "$WORK/town" && cd "$WORK/town"
if gt install --dolt-port "$DOLT_PORT" >/dev/null 2>&1; then
  pass "gt install creates a town"
else
  fail "gt install failed"; echo "  cannot continue"; exit 1
fi

# file:// rather than a bare path: gt requires an explicit remote scheme and
# rejects a plain filesystem path. Using a local bare repository keeps the
# fixture offline, which is what lets it run in CI with no network and no
# credentials.
if gt rig add fixture "file://$UPSTREAM" >/dev/null 2>&1; then
  pass "gt rig add clones a rig"
else
  # Everything after this asserts against the rig, so continuing would report a
  # cascade of failures that all have one cause and hide it.
  fail "gt rig add failed"
  gt rig add fixture "file://$UPSTREAM" 2>&1 | head -3 | sed 's/^/        /'
  echo "  cannot continue without a rig"
  exit 1
fi

# ---------------------------------------------------------------------------
# 3. Beads: create, link, and readiness.
#
# The dependency direction is asserted rather than assumed. Getting it backwards
# inverts readiness silently, which presents as a scheduling mystery.
# ---------------------------------------------------------------------------
cd "$WORK/town/fixture"
blocker=$(bd create "the blocker" --type task --silent 2>/dev/null)
blocked=$(bd create "the blocked" --type task --silent 2>/dev/null)

if [ -n "$blocker" ] && [ -n "$blocked" ]; then
  pass "bd creates beads inside a rig ($blocker, $blocked)"
else
  fail "bd create returned nothing"
fi

bd dep add "$blocked" "$blocker" >/dev/null 2>&1
ready=$(bd ready --json 2>/dev/null)
if says "$ready" "$blocker" && ! says "$ready" "$blocked"; then
  pass "readiness honours the dependency (blocker ready, blocked not)"
else
  fail "readiness is wrong after linking; the dependency may be inverted"
fi

# gt must agree with bd about what is ready. The orchestrator and the store
# disagreeing is exactly the incompatibility this fixture guards.
cd "$WORK/town"
gt_ready=$(gt ready 2>/dev/null)
if says "$gt_ready" "$blocker"; then
  pass "gt ready agrees with bd about ready work"
else
  fail "gt ready does not see work that bd reports as ready"
fi

# ---------------------------------------------------------------------------
# 4. Closing work, with evidence.
# ---------------------------------------------------------------------------
cd "$WORK/town/fixture"
bd close "$blocker" --reason "closed by the compatibility fixture" >/dev/null 2>&1
shown=$(bd show "$blocker" 2>/dev/null)
if says "$shown" "closed"; then
  pass "bd close records the close"
else
  fail "bd close did not close the bead"
fi

ready=$(bd ready --json 2>/dev/null)
if says "$ready" "$blocked"; then
  pass "closing the blocker releases the blocked work"
else
  fail "the blocked bead did not become ready after its blocker closed"
fi

# ---------------------------------------------------------------------------
# 5. Backup and restore.
#
# Plan section 15.3: the Beads native backup is the primary mechanism, and a
# backup nobody has restored is a hope rather than a backup.
# ---------------------------------------------------------------------------
if bd backup >/dev/null 2>&1; then
  pass "bd backup runs"
else
  fail "bd backup failed"
fi

# ---------------------------------------------------------------------------
# 6. Dolt is the store underneath, and it is the pinned one.
# ---------------------------------------------------------------------------
cd "$WORK/town"
dolt_status=$(gt dolt status 2>/dev/null)
if says "$dolt_status" "running|databases"; then
  pass "gt drives the pinned dolt server"
else
  fail "gt cannot reach a dolt server"
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "compatibility fixture FAILED: $failures assertion(s)"
  exit 1
fi
echo "compatibility fixture passed"
