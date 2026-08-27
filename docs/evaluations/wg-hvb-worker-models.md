# wg-hvb: which models can be workers?

**Ran:** 2026-08-27, corrected and re-run 2026-08-28 · **Harness:** `scripts/model_bakeoff.sh` · **Decides:** the model half of [ADR-0024](../adr/0024-multi-provider-agent-runtimes.md)

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

## Screens 2 and 3: every candidate passes

All six read the file and edited it exactly as told, and all six fixed the
off-by-one, verified on the edit rather than on the explanation.

The earlier version of this document reported four tool-use failures. Those were
`wg-azd` — a stale `PWD` inherited from the calling script, which OpenCode
believed over `getcwd()`. Not one of them was a model failure.

## Cost for the same verified outcome

Screen 3 is one task, so this ranks nothing. It is here because the spread is
larger than any argument about model quality is likely to be.

| Model | Cents | Calls |
|---|---:|---:|
| `@cf/zai-org/glm-5.3-flash` | 0.188 | 6 |
| `@cf/deepseek-ai/deepseek-v4-flash-0731` | 0.457 | 6 |
| `@cf/google/gemma-4-26b-a4b-it` | 0.515 | 9 |
| `@cf/moonshotai/kimi-k2.6` | 0.573 | 4 |
| `@cf/moonshotai/kimi-k2.7-code` | 0.885 | 5 |
| `anthropic/claude-sonnet-5` *(control)* | **60.215** | **47** |

Same task, same verified pass, and the default costs 320× the cheapest.

Read the call counts before drawing the obvious conclusion. Claude Code made 47
requests where the open-weight runs made four to nine, which is a difference in
how the harness works rather than in the model: it explores, reads more, and
checks itself. On a one-line boundary fix that is pure overhead. On an ambiguous
failure in an unfamiliar module it is most of the value, and this task cannot
tell the two apart.

What the number does justify is measuring properly rather than assuming the
expensive default is required. A task shape where a 0.19-cent model reliably
succeeds should not be run at 60 cents, and there are shapes like that in this
backlog.

## How this went wrong, four times

Every wrong answer had the same shape: **the harness broke in a way that looked
exactly like a model declining to work.** Worth recording, because that shape is
the thing to watch for rather than any individual cause.

1. Run on a laptop, where OpenCode merges a global config from
   `~/.config/opencode` that a cell user does not have. Five identical failures,
   *including the control*.
2. On the node, but the work directory was never created, so every log redirect
   failed and the runner never started.
3. Run directories reused between attempts, inheriting stale session state.
4. `wg-azd`: a stale `PWD` inherited from the calling script. Four models
   recorded as unable to use tools. All four use tools correctly.

The control caught the first three: a screen every candidate fails, control
included, is a broken screen. It did **not** catch the fourth, because Claude
Code ignores `PWD` and passed while everything else failed — which reads exactly
like a real result, and is the reason that one took eight ruled-out hypotheses.

A control tells you the screen works. It does not tell you the screen is
measuring what you think, and a control that differs from the candidates in the
way that matters will pass a broken screen.

The context column was wrong too, and differently: it was *recalled* rather than
measured, and three of seven figures were too small. Everything else in this
document came from a run. That column came from memory, and it was the only part
that a reader caught.

## What to do next

Screen 3 is one task and ranks nothing. What decides which model does which work
is cost per completed bead across real work of each shape, which needs a larger
and duller sample than anything here — several beads per shape, taken from work
that has already been done so the right answer is known.

Add `kimi-k3` once Unified Billing credits are loaded. It is the one model the
account cannot currently reach, and on published context it passes screen 1.

The harness is on the node as `/usr/local/bin/wg-bakeoff` and takes about four
minutes. `BAKEOFF_EXPLAIN=1` dumps each run's exact argv, environment and
working directory, which is what `wg-azd` needed and nobody had.
