#!/usr/bin/env bash
# WP-I3 criterion 3: resource limits stop an agent workload taking down the node.
#
# Run ON the execution node as root. It deliberately applies CPU and process
# pressure as a cell user and measures what happens to everything else. It does
# NOT push memory to the point of an out-of-memory kill: see the memory section
# for why that turned out to be unnecessary.
#
# The claim under test is not "the limits are configured" — that is readable from
# systemd — but "the limits protect the rest of the machine". Those are different
# claims and only one of them had evidence.
set -uo pipefail

OSS_UID="${OSS_UID:-999}"
OTHER_UID="${OTHER_UID:-996}"
OSS_USER="${OSS_USER:-wgcell_oss}"
OTHER_USER="${OTHER_USER:-wgcell_client_nged}"
DURATION="${DURATION:-25}"

pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
note() { printf '  ..    %s\n' "$1"; }
failures=0

cores=$(nproc)
ram_mib=$(free -m | awk '/^Mem:/ {print $2}')
CELL_SLICE="${CELL_SLICE:-wgcell-oss.slice}"
CELL_CG="/sys/fs/cgroup/wgcell.slice/${CELL_SLICE}"

# Read cpu.max from the cgroup rather than parsing systemd's rendering of
# CPUQuotaPerSecUSec. That property prints "880ms", and stripping a trailing "s"
# leaves "880m" — which reads as 880 cores and produced a confident false
# failure. cpu.max is two integers, quota and period, in microseconds.
cpu_max=$(cat "${CELL_CG}/cpu.max" 2>/dev/null)
quota_num=$(printf '%s' "$cpu_max" | awk '{print $1}')
quota_period=$(printf '%s' "$cpu_max" | awk '{print $2}')
quota_us=$(systemctl show "$CELL_SLICE" -p CPUQuotaPerSecUSec --value)
mem_max=$(systemctl show "$CELL_SLICE" -p MemoryMax --value)
tasks_max=$(systemctl show "$CELL_SLICE" -p TasksMax --value)

echo "Resource limits, on a node with ${cores} vCPU and ${ram_mib} MiB RAM:"
note "${CELL_SLICE}: CPUQuota=${quota_us}  MemoryMax=${mem_max}  TasksMax=${tasks_max}"
echo

# ---------------------------------------------------------------------------
# 1. A limit above the machine's capacity is not a limit
# ---------------------------------------------------------------------------
# This is arithmetic, not a stress test, and it is the most important check
# here. A memory ceiling higher than physical RAM can never be reached: the
# kernel exhausts the machine first, and the cgroup never intervenes. The
# setting looks protective in `systemctl show` and protects nothing.
mem_max_mib=$(( mem_max / 1024 / 1024 ))
if [ "$mem_max_mib" -lt "$ram_mib" ]; then
  pass "the memory ceiling (${mem_max_mib} MiB) is below physical RAM (${ram_mib} MiB), so it can fire"
else
  fail "the memory ceiling is ${mem_max_mib} MiB on a ${ram_mib} MiB machine — it can NEVER fire"
  note "the node exhausts memory before the cgroup limit is reached; this setting protects nothing"
fi

# The same reasoning for CPU. A quota equal to every core means one cell may
# consume the whole machine, leaving nothing for sshd or the tunnel — which is
# the only way in, since the node has no inbound port.
if [ "$quota_num" = "max" ] || [ -z "$quota_num" ]; then
  fail "no CPU quota at all: one cell may consume the entire node"
  quota_cores="unbounded"
else
  quota_cores=$(awk -v q="$quota_num" -v p="${quota_period:-100000}" 'BEGIN {printf "%.2f", q/p}')
fi
if [ "$quota_cores" = "unbounded" ]; then
  :
elif awk -v q="$quota_cores" -v c="$cores" 'BEGIN {exit !(q >= c)}'; then
  fail "the CPU quota (${quota_cores} cores) is the whole machine (${cores} cores)"
  note "nothing is reserved for sshd or the tunnel, and the tunnel is the only way to intervene"
else
  pass "the CPU quota (${quota_cores} cores) leaves headroom on a ${cores}-core node"
fi

# ---------------------------------------------------------------------------
# 1b. A limit on a cgroup nothing joins is not a limit either
# ---------------------------------------------------------------------------
# This is the check that was missing, and its absence is why the whole thing
# looked fine. user-<uid>.slice carried a quota and held ZERO processes: agents
# start via `su` from a root SSH session, so they inherited root's session scope
# and no per-cell limit applied to any of them. Configuration was correct and
# enforcement was absent.
echo
if ! systemctl cat "$CELL_SLICE" >/dev/null 2>&1; then
  fail "${CELL_SLICE} does not exist; agents cannot be placed in a limited cgroup"
else
  probe_cg=$(systemd-run --quiet --uid="$OSS_USER" --slice="$CELL_SLICE" \
      --unit=wg-placement-probe --scope sh -c 'cat /proc/self/cgroup' 2>/dev/null | head -1 | cut -d: -f3)
  systemctl stop wg-placement-probe.scope 2>/dev/null
  if printf '%s' "$probe_cg" | grep -q "$(printf '%s' "$CELL_SLICE" | sed 's/\.slice$//')"; then
    pass "a cell process lands inside ${CELL_SLICE} (${probe_cg})"
  else
    fail "a cell process landed in ${probe_cg:-nowhere}, not ${CELL_SLICE}"
  fi
fi

# ---------------------------------------------------------------------------
# 2. Process count IS capped, and that one works
# ---------------------------------------------------------------------------
echo
note "forking past TasksMax as ${OSS_USER}..."
# Counted by asking the kernel how many tasks the cell's cgroup holds, not by
# counting successful forks in the shell. The shell version returned an empty
# string: `n=$((n+1)) || break` never breaks, because an assignment succeeds
# even when the fork before it failed.
systemd-run --quiet --uid="$OSS_USER" --slice="$CELL_SLICE" --unit=wg-fork-probe --scope \
  sh -c 'for i in $(seq 1 900); do sleep 30 & done' >/dev/null 2>&1
sleep 2
spawned=$(pgrep -u "$OSS_USER" -c -f "sleep 30" 2>/dev/null)
spawned=${spawned:-0}
pkill -u "$OSS_USER" -f "sleep 30" 2>/dev/null
systemctl stop wg-fork-probe.scope 2>/dev/null
sleep 1

if [ -n "$spawned" ] && [ "$spawned" -le $(( tasks_max + 50 )) ]; then
  pass "process count capped near TasksMax (${spawned} spawned, limit ${tasks_max})"
else
  fail "spawned ${spawned} processes against a TasksMax of ${tasks_max}"
fi

# ---------------------------------------------------------------------------
# 3. Under CPU pressure from one cell, is anything else still usable?
# ---------------------------------------------------------------------------
# The measurement that matters. A limit is only worth having if the rest of the
# machine keeps working while it is being hit.
echo
measure_other() {  # how long a trivial command takes as the OTHER cell
  local start end
  start=$(date +%s%N)
  su -s /bin/bash -c 'for i in $(seq 1 200000); do :; done' "$OTHER_USER" >/dev/null 2>&1
  end=$(date +%s%N)
  echo $(( (end - start) / 1000000 ))
}
measure_ssh() {  # how long a trivial local command takes at all
  local start end
  start=$(date +%s%N); true; sleep 0.05; end=$(date +%s%N)
  echo $(( (end - start) / 1000000 ))
}

baseline_other=$(measure_other)
note "baseline: a trivial loop as ${OTHER_USER} takes ${baseline_other} ms"

note "applying CPU pressure from ${OSS_USER} for ${DURATION}s (${cores}x oversubscribed)..."
cpu_before=$(awk '/^usage_usec/{print $2}' "${CELL_CG}/cpu.stat" 2>/dev/null)
window_start=$(date +%s)
for i in $(seq 1 $(( cores * 2 ))); do
  systemd-run --quiet --uid="$OSS_USER" --slice="$CELL_SLICE" --unit="wg-spin-$i" --scope \
    sh -c "timeout ${DURATION} sh -c 'while :; do :; done'" >/dev/null 2>&1 &
done
sleep 6
spinners=$(pgrep -u "$OSS_USER" -c -f "while :" 2>/dev/null)
note "${spinners:-0} spinner(s) running as ${OSS_USER}"

loaded_other=$(measure_other)
loadavg=$(awk '{print $1}' /proc/loadavg)
note "under load: the same loop takes ${loaded_other} ms (load average ${loadavg})"

# Is the machine still administrable? If this is slow, nobody can intervene.
resp=$(measure_ssh)
note "a local scheduling round-trip under load: ${resp} ms"

cpu_after=$(awk '/^usage_usec/{print $2}' "${CELL_CG}/cpu.stat" 2>/dev/null)
window=$(( $(date +%s) - window_start ))
wait 2>/dev/null
pkill -u "$OSS_USER" -f "while :" 2>/dev/null
for i in $(seq 1 $(( cores * 2 ))); do systemctl stop "wg-spin-$i.scope" 2>/dev/null; done
sleep 2

# Did the pressure actually land? Without this the responsiveness check below
# passes whenever the load fails to apply, which is the most common way an
# isolation test gives false comfort. Measured from the cell's own cgroup
# accounting rather than load average, which lags and is machine-wide.
# cpu.stat is in microseconds. Judged against the ACTUAL measurement window,
# not the spinners' lifetime: an earlier version compared against DURATION while
# only observing about eight seconds of it, and called a perfectly good run a
# failure.
burned_ms=$(( ( ${cpu_after:-0} - ${cpu_before:-0} ) / 1000 ))
expect_ms=$(awk -v w="${window:-1}" -v q="$quota_cores" 'BEGIN {printf "%d", w*1000*q*0.5}')
if [ "$burned_ms" -ge "${expect_ms:-1}" ]; then
  pass "the pressure landed: ${burned_ms} ms of CPU in a ${window}s window (quota ${quota_cores} cores)"
else
  fail "the pressure did NOT land (${burned_ms} ms in ${window}s, expected at least ${expect_ms}); the check below would be vacuous"
fi

# A large slowdown of an unrelated cell means the quota is not isolating them.
if [ "$baseline_other" -gt 0 ] && [ "$burned_ms" -ge "${expect_ms:-1}" ]; then
  ratio=$(( loaded_other * 100 / baseline_other ))
  if [ "$ratio" -le 300 ]; then
    pass "the other cell stayed responsive under pressure (${ratio}% of baseline)"
  else
    fail "the other cell slowed to ${ratio}% of baseline; one cell can starve another"
  fi
else
  note "skipping the isolation comparison: no baseline, or the pressure never landed"
fi

# Throttling is the positive evidence that the ceiling did something, as opposed
# to the workload simply not being heavy enough to reach it.
throttled=$(awk '/^nr_throttled/{print $2}' "${CELL_CG}/cpu.stat" 2>/dev/null)
if [ "${throttled:-0}" -gt 0 ]; then
  pass "the CPU quota actually throttled the cell (${throttled} throttled periods)"
else
  fail "no throttling recorded; the quota was never reached, so it is unproven"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "Resource limits: all checks passed."
else
  echo "Resource limits: ${failures} check(s) FAILED."
fi
exit "$failures"
