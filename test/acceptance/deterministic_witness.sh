#!/usr/bin/env bash
# The witness watches without inference (ADR-0019 acceptance).
#
# The claim under test is not "the witness works" but "the witness works and
# costs nothing to run". Those need separate evidence, because a health monitor
# that quietly starts a Claude session is indistinguishable from one that does
# not until the bill arrives.
#
# Run ON the execution node as root. Unlike the other acceptance scripts this
# one spends no money at all, which is the point.
set -uo pipefail

CELL=/srv/cells/oss
TOWN=$CELL/town
SETTINGS=$TOWN/settings/config.json
WITNESS=/usr/local/bin/wg-witness
failures=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

run() {
  su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=$CELL
cd $TOWN && $1" wgcell_oss 2>&1
}
as_cell() {
  su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=$CELL
$1" wgcell_oss 2>&1
}
# `pgrep -c` prints 0 AND exits non-zero when nothing matches, so the obvious
# `pgrep -c ... || echo 0` prints "0" twice and every arithmetic test on the
# result becomes a syntax error. Take the output and default it instead.
claude_count() {
  local n
  n=$(pgrep -u wgcell_oss -c -f claude 2>/dev/null)
  printf '%s' "${n:-0}"
}

echo "Deterministic witness:"

# ---------------------------------------------------------------------------
# 1. The stock patrol is off
# ---------------------------------------------------------------------------
#
# Both witnesses running would be two things nuking the same sandboxes, and the
# saving would be zero because the expensive one would still be up.
if [ ! -f "$SETTINGS" ]; then
  fail "no town settings at $SETTINGS"
else
  if python3 -c "
import json, sys
d = json.load(open('$SETTINGS'))
sys.exit(0 if 'witness' in d.get('disabled_patrols', []) else 1)
" 2>/dev/null; then
    pass "the Gas Town witness patrol is disabled in town settings"
  else
    fail "disabled_patrols does not contain 'witness'; both witnesses would run"
  fi
fi

# ---------------------------------------------------------------------------
# 2. The binary is deployed and cannot be edited by the cell it watches
# ---------------------------------------------------------------------------
if [ ! -x "$WITNESS" ]; then
  fail "$WITNESS is not installed"
else
  pass "the witness binary is installed"
  owner=$(stat -c '%U' "$WITNESS" 2>/dev/null)
  writable=$(stat -c '%A' "$WITNESS" 2>/dev/null | cut -c9)
  if [ "$owner" = "root" ] && [ "$writable" != "w" ]; then
    pass "the witness is root-owned and not writable by the cell it watches"
  else
    fail "the witness is owned by '$owner' with other-write '$writable'; an agent could rewrite its own monitor"
  fi
fi

# ---------------------------------------------------------------------------
# 3. Dolt up, so gt can answer. This is the witness's only dependency.
# ---------------------------------------------------------------------------
run "gt dolt start >/dev/null 2>&1"
for _ in $(seq 1 12); do
  run "gt dolt status 2>/dev/null" | grep -qi running && break
  sleep 5
done

before=$(claude_count)

# ---------------------------------------------------------------------------
# 4. A pass decides something
# ---------------------------------------------------------------------------
#
# -dry-run so the acceptance test never destroys a sandbox. What is being
# checked is that decisions are produced and explained, not that nuking works;
# the nuke path is covered by the property test in internal/witness.
out=$(as_cell "$WITNESS -town $TOWN -cell oss -once -dry-run" 2>&1)
rc=$?

if [ $rc -ne 0 ]; then
  fail "the witness exited $rc"
  printf '%s\n' "$out" | tail -5
else
  pass "a pass completed"
fi

# Checked by exit code and not by grepping for absence: a pass that produced no
# output at all would otherwise look like a pass that found nothing wrong.
if printf '%s' "$out" | grep -q '"msg":"pass complete"'; then
  pass "the pass reported a tally"
else
  fail "no tally line; the witness did not finish a pass"
fi

# Every decision must carry its basis. An unexplained health action cannot be
# reviewed, and an operator woken by one needs to see what the machine saw.
unexplained=$(printf '%s' "$out" | grep '"msg":"escalating"' | grep -cv '"basis":"[^"]' || true)
if [ "${unexplained:-0}" -eq 0 ]; then
  pass "every escalation carries its basis"
else
  fail "$unexplained escalation(s) with no basis"
fi

# ---------------------------------------------------------------------------
# 5. No inference. The whole point.
# ---------------------------------------------------------------------------
after=$(claude_count)
# Checked before use. A non-numeric count means the probe itself is broken, and
# a broken probe that silently skips its assertion is worse than a failing one —
# an earlier version of this script reported "all checks passed" while this
# comparison was throwing a syntax error.
case "$before$after" in
  *[!0-9]*)
    fail "could not count agent processes (before='$before' after='$after')" ;;
  *)
    if [ "$after" -le "$before" ]; then
      pass "the pass started no agent session (claude processes $before -> $after)"
    else
      fail "the pass started $((after - before)) claude process(es); it is supposed to make no model calls"
    fi ;;
esac

# The witness never invokes an agent binary, so its process tree is the check
# that survives someone adding a model call later.
if printf '%s' "$out" | grep -qiE '"(model|tokens|anthropic|gateway)"'; then
  fail "the witness logged model activity"
else
  pass "the pass logged no model activity"
fi

# ---------------------------------------------------------------------------
# 6. Repeating a pass does not change the world
# ---------------------------------------------------------------------------
#
# Idempotence matters more here than usual: the witness runs continuously, so a
# pass with side effects would apply them every interval.
out2=$(as_cell "$WITNESS -town $TOWN -cell oss -once -dry-run" 2>&1)
tally1=$(printf '%s' "$out"  | grep -o '"escalated":[0-9]*' | tail -1)
tally2=$(printf '%s' "$out2" | grep -o '"escalated":[0-9]*' | tail -1)
if [ -n "$tally1" ] && [ "$tally1" = "$tally2" ]; then
  pass "a repeated pass reaches the same conclusion ($tally1)"
else
  fail "the tally changed between identical passes: '$tally1' then '$tally2'"
fi

# ---------------------------------------------------------------------------
# 7. The escalation path actually reaches the control plane
# ---------------------------------------------------------------------------
#
# The service ran for hours deciding correctly and reporting nothing, because
# it read WG_SERVICE_TOKEN_* while the cell supplies WG_ACCESS_CLIENT_*. It
# looked exactly like a healthy witness with nothing to say. Checked here
# because a findings pipeline that reaches nobody is the failure this whole
# component exists to avoid.
if systemctl is-active --quiet wg-witness@oss.service; then
  pass "the witness service is running"
  recent=$(journalctl -u wg-witness@oss.service -n 200 --no-pager -o cat 2>/dev/null)
  if printf '%s' "$recent" | grep -q "could not report to the control plane"; then
    fail "the witness cannot reach the control plane; escalations stop at the journal"
    printf '%s' "$recent" | grep "could not report" | tail -1 | sed 's/^/        /'
  else
    pass "no control-plane reporting failures in the recent journal"
  fi
else
  fail "wg-witness@oss.service is not running"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "Deterministic witness: all checks passed."
else
  echo "Deterministic witness: $failures check(s) FAILED."
fi
exit "$failures"
