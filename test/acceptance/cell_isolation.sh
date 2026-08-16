#!/usr/bin/env bash
# Prove that one execution cell cannot reach another, or the control plane.
#
# This is the WP-E1 acceptance criterion, and it is written as an executable
# check rather than a description because the properties it asserts are exactly
# the ones that erode silently: a mode changed to make something work, a user
# added to a group, a directory created by a later role with a friendlier
# umask. Nothing in the system fails visibly when isolation weakens.
#
# Run ON the execution node, as root. Root is required to become each cell user;
# every assertion is then made AS that user, which is the perspective that
# matters.
set -uo pipefail

CELLS_ROOT=/srv/cells
A=wgcell_oss                # the untrusted, busiest cell
B=wgcell_client_nged        # the restricted client cell
B_HOME="$CELLS_ROOT/client-nged"

failures=0
LAST_OUTPUT=""
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# Run a command as the OSS cell user. The cell shell is nologin, so su -s is
# used deliberately: this tests filesystem permissions, not shell availability.
as_a() { su -s /bin/bash -c "$1" "$A" 2>&1; }

# Capture, then match. Piping into grep would look natural but is wrong under
# `set -o pipefail`: the denied command exits non-zero, so the pipeline reports
# failure even when grep matched, and every successful denial reads as a failed
# assertion. Found the honest way — the first run reported five failures whose
# own output said "Permission denied".
denied() {
  local out
  out=$(as_a "$1")
  if printf '%s' "$out" | grep -qiE "permission denied|no such file|cannot open|refused|unreachable|timed out|no route"; then
    return 0
  fi
  LAST_OUTPUT="$out"
  return 1
}

echo "Cell isolation, asserted as $A:"

# 1. Reading another cell's files.
if denied "cat $B_HOME/.credentials/env"; then
  pass "cannot read the restricted cell's credentials"
else
  fail "READ another cell's credentials: $LAST_OUTPUT"
fi

# 2. Entering another cell's home.
if denied "cd $B_HOME && ls"; then
  pass "cannot enter the restricted cell's home"
else
  fail "ENTERED another cell's home: $LAST_OUTPUT"
fi

# 3. Enumerating the cells.
#
# Listing /srv/cells is not harmless: the directory names are client names, so
# enumeration alone discloses who Datopian works with. This is why the root is
# 0711 (traversable) rather than 0755 (listable).
if denied "ls $CELLS_ROOT"; then
  pass "cannot enumerate the cells (directory names are client names)"
else
  fail "ENUMERATED the cells: $LAST_OUTPUT"
fi

# 4. Cloud metadata.
#
# The Hetzner metadata service hands out the node's identity and any injected
# user data to anything that can reach it. An agent that can read it can often
# escalate beyond the node entirely.
meta=$(as_a "timeout 5 curl -s -o /dev/null -w %{http_code} http://169.254.169.254/hetzner/v1/metadata")
if [ "$meta" != "200" ]; then
  pass "cannot reach cloud metadata (got '${meta:-blocked}')"
else
  fail "REACHED cloud metadata; the node's identity is exposed to agents"
fi

# 5. Control-plane database.
#
# The execution node must not be able to talk to Postgres on the control node
# at all. Agent work reaches the domain model through the control API, which
# authenticates and applies row-level security; a direct connection bypasses
# every one of those checks.
if as_a "timeout 5 bash -c 'echo > /dev/tcp/10.10.1.2/5432'" >/dev/null 2>&1; then
  fail "OPENED a connection to the control-plane database port"
else
  pass "cannot open the control-plane database port"
fi

# 6. Writing outside the cell.
if denied "touch /srv/cells/probe"; then
  pass "cannot write into the cells root"
else
  rm -f /srv/cells/probe
  fail "WROTE into the cells root: $LAST_OUTPUT"
fi

# 7. Confinement is available and actually confines.
#
# bwrap adds mount, PID and IPC namespace separation inside the cell user. The
# assertion is not that the binary exists but that a process inside it cannot
# see the other cell's home even when the path is handed to it directly.
if command -v bwrap >/dev/null; then
  out=$(as_a "bwrap --ro-bind /usr /usr --ro-bind /lib /lib --ro-bind /lib64 /lib64 \
        --proc /proc --dev /dev --unshare-pid --unshare-ipc --unshare-uts \
        --die-with-parent /bin/ls $B_HOME" )
  if echo "$out" | grep -qiE "no such file|permission denied"; then
    pass "a confined process cannot see the restricted cell's home"
  else
    fail "a confined process listed another cell's home: $(echo "$out" | head -1)"
  fi
else
  fail "bubblewrap is not installed; spawn confinement is unavailable"
fi

# 8. Seeing another cell's processes.
#
# Gas Town passes credentials as tmux -e arguments, so a process command line
# contains the cell's gateway token. Without hidepid every cell can read every
# other cell's, which defeats per-cell credential separation entirely — and the
# earlier version of this file checked the filesystem and the network but never
# process visibility, so it passed while this was wide open.
# Start a probe owned by the OTHER cell and note its PID.
#
# Searching by a marker string does not work: the searching shell's own command
# line contains the marker, so it matches itself and the assertion fails while
# the system is correct. Ask about one specific PID instead.
probe_pid=$(su -s /bin/bash -c 'nohup sleep 30 >/dev/null 2>&1 & echo $!' "$B" 2>/dev/null | tail -1)

if [ -z "${probe_pid:-}" ]; then
  fail "could not start a probe process as the restricted cell"
else
  sleep 1
  visible=$(as_a "cat /proc/$probe_pid/cmdline 2>&1; ls -d /proc/$probe_pid 2>&1")
  kill "$probe_pid" 2>/dev/null
  if printf '%s' "$visible" | grep -qiE "no such file|permission denied|cannot access"; then
    pass "cannot see another cell's process (its command line carries the token)"
  else
    fail "READ another cell's process at /proc/$probe_pid: $visible"
  fi
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "cell isolation FAILED: $failures assertion(s)"
  exit 1
fi
echo "cell isolation verified"
