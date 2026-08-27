# wg-hvb: which models can be workers?

**Ran:** 2026-08-27 · **Harness:** `scripts/model_bakeoff.sh` · **Decides:** the model half of [ADR-0024](../adr/0024-multi-provider-agent-runtimes.md)

Three screens, cheapest first, because each costs an order of magnitude more than
the one before and most candidates die early:

1. **context** — arithmetic, no calls at all
2. **tool use** — one run each: read a file and edit it when told to
3. **real work** — one run each: fix an off-by-one, scored on the edit

The order is the finding from [wg-uhj](../spikes/wg-uhj-opencode-gateway.md).
OpenCode's agent system prompt is about 19,900 tokens, so context filters models
before quality does — and it filters absolutely.

## Screen 1: context

A worker needs room for the prompt **and** the work, so the bar is four times the
prompt: 79,600 tokens.

Context figures are Cloudflare's published ones, read from
`developers.cloudflare.com/workers-ai/models/<slug>` on 2026-08-28.

| Model | Context | |
|---|---:|---|
| `@cf/deepseek-ai/deepseek-v4-flash-0731` | 1,310,720 | pass |
| `@cf/zai-org/glm-5.3-flash` | 1,048,576 | pass |
| `@cf/moonshotai/kimi-k2.7-code` | 262,144 | pass |
| `@cf/moonshotai/kimi-k2.6` | 262,144 | pass |
| `@cf/google/gemma-4-26b-a4b-it` | 256,000 | pass |
| `anthropic/claude-sonnet-5` *(control)* | 200,000 | pass |
| `@cf/qwen/qwen3-30b-a3b-fp8` | 32,768 | **drop** |

**This table was wrong when first published, and the correction is the more
useful finding.** Three of seven figures were too small — `gemma-4-26b` recorded
as 16,384 against a real 256,000, `deepseek-v4-flash` as 131,072 against
1,310,720 from a dropped digit, and `glm-5.3-flash` likewise. They were written
from assumption rather than read from the documentation, and `gemma-4-26b` was
dropped from the field because of it.

So the screen is much less selective than it first appeared: **one candidate of
six fails on context**, not two. What survives is the shape of the finding rather
than its severity — `llama-3.3-70b` has a 24,000-token window, confirmed both by
the documentation and by the error it returns, and is the best T0 classifier we
measured. It cannot be a worker. That remains true and remains the reason to
screen on context before quality.

The lesson is narrower and worth more than the table: a number nobody checked
reads exactly like a measurement. Everything else in this document was measured;
this column was recalled, and it was the only part that was wrong.

## Screens 2 and 3: blocked, and not by the models

The control passes both: `claude-sonnet-5` edits the file when told to, and
fixes the off-by-one. So the harness works.

Every non-Anthropic candidate fails with `Error: Session not found`, thrown by
OpenCode about seven seconds in, before the model does anything. **That is not a
model result and must not be read as one.** Invoked directly from a shell with
the identical configuration, `kimi-k2.7-code` reads the file, writes it, and
verifies its own work.

Tracked as `wg-azd`. Ruled out so far, each tested individually on the execution
node as the cell user, all of which work from a shell: the deny-first permission
block; OpenCode's shared session store; an isolated or missing `XDG_DATA_HOME`;
the working directory being under the cell; stdin closed; the child in its own
process group; output to a file rather than a pipe; and the exact generated
config, byte for byte, with the same environment and working directory.

The difference is something `wg-runner` does to the child that a shell does not,
and it only matters once a tool is involved — a run needing no tools succeeds
through `wg-runner` and is attributed normally.

## How this went wrong first, and what it cost

Three earlier attempts produced confident, wrong answers. Each is worth
recording, because the failure mode was the same every time: **the harness broke
in a way that looked exactly like a model declining to work.**

The first run was on a laptop, where OpenCode merges a global config from
`~/.config/opencode` that a cell user does not have. Five identical failures —
*including the control*. The second and third were on the node but the work
directory was never created, so every log redirect failed and the runner never
started; then run directories were reused between attempts, and stale session
state produced the same message.

The control is what caught all three. A screen every candidate fails, control
included, is a broken screen — and without a known-good entry in the list there
is nothing to distinguish that from a genuinely poor field.

## What to do next

`wg-azd` first: it blocks the only screens that can rank models. Then re-run —
the harness is on the node as `/usr/local/bin/wg-bakeoff` and takes about two
minutes.

Add `kimi-k3` to the candidate list once Unified Billing credits are loaded. It
is the one model the account currently cannot reach (403), and on published
context it would pass screen 1 comfortably.

And treat screen 3 as a placeholder. One off-by-one is a smoke test, not a
ranking. What decides which model does which work is cost per *completed bead*
across real work of each shape — which needs `wg-azd` fixed and then a larger,
duller sample than anything in this document.
