#!/usr/bin/env bash
# Which models can actually do the work? (wg-hvb, ADR-0024)
#
#   scripts/on.sh staging execution '/usr/local/bin/wg-bakeoff'
#
# ON THE EXECUTION NODE, not a workstation, and that is not a detail. OpenCode
# merges a global config from ~/.config/opencode into every run. A developer
# machine has one; the cell user does not, so a bake-off run on a laptop
# measures the laptop. The first attempt did exactly that and produced five
# identical failures including the control — which is how you know the harness
# was wrong rather than the models.
#
# Three screens, cheapest first, because each is an order of magnitude dearer
# than the one before and most candidates die early:
#
#   1. context   arithmetic, no calls at all
#   2. tool use  one run each: can it read and write a file when told to
#   3. real work one run each: fix a failing test, verified by running it
#
# The order is the finding from wg-uhj. OpenCode's agent system prompt is about
# 19,900 tokens, so context filters models before quality does — and it filters
# them absolutely. llama-3.3-70b is a capable classifier and cannot host the
# prompt at all, and those two facts have nothing to do with each other.
#
# Screen 3 is scored on the EDIT, not on the answer. A model that
# explains a correct fix and edits nothing has done no work, and prose is exactly
# what a language model is best at producing regardless of whether it worked.
set -uo pipefail

# Runs against the real cell, as the cell user, with the token the cell's own
# agents use — so what is measured is the environment work actually happens in.
CELL="${WG_CELL_ROOT:-/srv/cells/oss}"
WORK="${BAKEOFF_DIR:-$(mktemp -d)}"
RUNNER="${RUNNER_BIN:-/usr/local/bin/wg-runner}"
STAMP="${BAKEOFF_STAMP:-bakeoff}"

# Read from the cell's own settings rather than passed in, on the argument
# scripts/dispatch_bead.sh makes: a run must not reach a different gateway than
# the cell's agents do.
WG_AI_GATEWAY_TOKEN="$(python3 -c '
import json,re,sys
s=json.load(open(sys.argv[1]))
h=s.get("env",{}).get("ANTHROPIC_CUSTOM_HEADERS","")
m=re.search(r"cf-aig-authorization: Bearer (\S+)",h)
print(m.group(1) if m else "")' "$CELL/.claude/settings.json")"
if [ -z "$WG_AI_GATEWAY_TOKEN" ]; then
  echo "no gateway token in $CELL/.claude/settings.json" >&2
  exit 1
fi
export WG_AI_GATEWAY_TOKEN

# OpenCode's system prompt, measured in wg-uhj. A model needs room for the
# prompt AND the work, so the bar is four times it — enough for a couple of
# files and some iteration, which is the least a worker does.
PROMPT_TOKENS=19900
MIN_CONTEXT=$((PROMPT_TOKENS * 4))

# Candidates, with the context each publishes. Every one returned 200 through the
# staging oss gateway on 2026-08-27; kimi-k3 is excluded because the account
# cannot reach it until Unified Billing credits are loaded.
# Context windows are Cloudflare's published figures, read from
# developers.cloudflare.com/workers-ai/models/<slug> on 2026-08-28. They are
# copied here rather than fetched, because a planner that makes a network call
# has a new failure mode — but copied numbers rot, so the date is the point.
#
# Three of these were wrong when this script was first written, all of them too
# small, and one of the two models the screen dropped was dropped because of it.
# A number nobody checked reads exactly like a measurement.
CANDIDATES="
workers-ai/@cf/moonshotai/kimi-k2.7-code:262144
workers-ai/@cf/moonshotai/kimi-k2.6:262144
workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731:1310720
workers-ai/@cf/zai-org/glm-5.3-flash:1310720
workers-ai/@cf/google/gemma-4-26b-a4b-it:256000
workers-ai/@cf/qwen/qwen3-30b-a3b-fp8:32768
"
# The control. If this fails a screen, the screen is wrong.
CONTROL="anthropic/claude-sonnet-5:200000"



say() { printf '%s\n' "$*"; }
rule() { printf '%s\n' "----------------------------------------------------------------"; }

# An optional filter, so one candidate can be screened without paying for six.
#
#   wg-bakeoff glm-5.3            # that candidate and the control
#   wg-bakeoff                    # everything
#
# The control is never filtered out. A screen without it cannot distinguish a
# model that failed from a harness that was broken, which is exactly what
# happened the first time this was run on a laptop: five identical failures,
# including the control, and the harness was the fault.
if [ "$#" -gt 0 ]; then
  filtered=""
  for candidate in $CANDIDATES; do
    case "$candidate" in *"$1"*) filtered="$filtered$candidate
";; esac
  done
  if [ -z "$filtered" ]; then
    say "no candidate matches '$1'; the list is:"
    for candidate in $CANDIDATES; do say "  ${candidate%%:*}"; done
    exit 2
  fi
  CANDIDATES="$filtered"
fi

# run <bead> <model> <runtime> <instructions> -> writes the run's output, returns rc
run_agent() {
  local bead="$1" model="$2" rt="$3" instr="$4"
  WG_AI_GATEWAY_TOKEN="$WG_AI_GATEWAY_TOKEN" "$RUNNER" \
    -bead "$bead" -cell oss -rig sandbox -cell-root "$CELL" \
    -instructions "$instr" -deadline 6m -keep ${BAKEOFF_EXPLAIN:+-explain} \
    -runtime "$rt" -model "$model" >"$WORK/$bead.log" 2>&1
}

# Created here, because BAKEOFF_DIR overrides the mktemp default and nothing
# else makes it. Without it every run's log redirect fails, the runner never
# starts, and each model looks like it declined to edit the file — which is
# exactly what the first node run reported.
mkdir -p "$WORK"

say "bake-off working directory: $WORK"
say "cell: $CELL"

# --- Screen 1: context -------------------------------------------------------
rule; say "screen 1 — context (needs > ${MIN_CONTEXT} for a ${PROMPT_TOKENS}-token prompt plus work)"; rule
SURVIVORS=""
for entry in $CANDIDATES $CONTROL; do
  model="${entry%:*}"; ctx="${entry##*:}"
  if [ "$ctx" -ge "$MIN_CONTEXT" ]; then
    printf '  pass  %-52s %s\n' "$model" "$ctx"
    SURVIVORS="$SURVIVORS $entry"
  else
    printf '  DROP  %-52s %s — cannot host the prompt and still work\n' "$model" "$ctx"
  fi
done

# --- Screen 2: tool use ------------------------------------------------------
rule; say "screen 2 — tool use (read a file, edit it, exactly as told)"; rule
STAGE2=""
for entry in $SURVIVORS; do
  model="${entry%:*}"
  slug="$(printf '%s' "$model" | tr -c 'a-zA-Z0-9' '-' | sed 's/--*/-/g;s/^-//;s/-$//')"
  bead="${STAMP}-tools-${slug}"
  rt=opencode; case "$model" in anthropic/*) rt=claude ;; esac

  # Removed first. A run directory left by an earlier attempt carries OpenCode
  # session state for that path, and the next run against it dies with
  # "Session not found" before reaching the model — which reads as the model
  # declining to use tools. Four candidates were recorded as tool-use failures
  # that way, and all four use tools correctly.
  run="$CELL/runs/$bead"
  rm -rf "$run"; mkdir -p "$run"
  printf 'REPLACE_ME\n' > "$run/target.txt"

  run_agent "$bead" "$model" "$rt" \
    "There is a file called target.txt in your working directory. Replace its entire contents with exactly the single word DONE and nothing else. Do not explain, just make the edit."

  got="$(tr -d ' \n\r' < "$run/target.txt" 2>/dev/null || echo MISSING)"
  if [ "$got" = "DONE" ]; then
    printf '  pass  %-52s edited the file\n' "$model"
    STAGE2="$STAGE2 $entry"
  elif grep -q "Session not found\|Error: " "$WORK/$bead.log" 2>/dev/null; then
    printf '  ERROR %-52s harness: %s\n' "$model" "$(grep -o 'Error: .*' "$WORK/$bead.log" | tail -1 | cut -c1-40)"
  else
    printf '  DROP  %-52s target.txt = %q\n' "$model" "$got"
  fi
done

# --- Screen 3: real work -----------------------------------------------------
rule; say "screen 3 — real work (fix an off-by-one; scored on the edit)"; rule
for entry in $STAGE2; do
  model="${entry%:*}"
  slug="$(printf '%s' "$model" | tr -c 'a-zA-Z0-9' '-' | sed 's/--*/-/g;s/^-//;s/-$//')"
  bead="${STAMP}-work-${slug}"
  rt=opencode; case "$model" in anthropic/*) rt=claude ;; esac

  run="$CELL/runs/$bead"
  rm -rf "$run"; mkdir -p "$run"
  # An off-by-one, which is a real bug shape and not a puzzle: the boundary is
  # wrong, so the function admits a value it should reject.
  cat > "$run/window.go" <<'GO'
package bakeoff

// InWindow reports whether n is inside the half-open window [lo, hi):
// lo is included and hi is NOT.
func InWindow(n, lo, hi int) bool {
	return n >= lo && n <= hi
}
GO
  cat > "$run/window_test.go" <<'GO'
package bakeoff

import "testing"

func TestInWindowExcludesTheUpperBound(t *testing.T) {
	if !InWindow(5, 5, 10) {
		t.Error("lo must be included")
	}
	if !InWindow(9, 5, 10) {
		t.Error("9 is inside [5,10)")
	}
	if InWindow(10, 5, 10) {
		t.Error("hi must be excluded from a half-open window")
	}
}
GO

  run_agent "$bead" "$model" "$rt" \
    "The test in window_test.go fails. Read window.go and window_test.go, work out why, and edit window.go so the test passes. Change only window.go. Do not change the test."

  # Scored on the edit, not by compiling: the execution node has no Go
  # toolchain, and the fix to a half-open window is unambiguous enough to check
  # textually. `n <= hi` is the bug; `n < hi` is the fix.
  body="$(cat "$run/window.go" 2>/dev/null || echo MISSING)"
  if printf '%s' "$body" | grep -q 'n < hi' && ! printf '%s' "$body" | grep -q 'n <= hi'; then
    printf '  PASS  %-52s fixed the boundary\n' "$model"
  elif printf '%s' "$body" | grep -q 'n <= hi'; then
    printf '  fail  %-52s left the off-by-one in place\n' "$model"
  else
    printf '  fail  %-52s window.go is not recognisable\n' "$model"
  fi
done

rule
say "runs are under $WORK; cost lands in usage_records against beads named ${STAMP}-*"
say "import it with: scripts/on.sh staging control 'systemctl start wg-costimport.service'"
