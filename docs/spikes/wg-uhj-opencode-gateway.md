# wg-uhj: can OpenCode carry gateway attribution?

**Ran:** 2026-08-27 · **Result:** pass · **Decides:** [ADR-0024](../adr/0024-multi-provider-agent-runtimes.md)

The question was never "can OpenCode reach another model" — it obviously can. It was
whether a run through it still lands in `usage_records` with the role, cell and bead the
budget gate needs. A run that reaches a model but arrives untagged is worse than one that
fails: it looks like success from outside every budget.

It does. This is the configuration that worked, so nobody has to derive it again.

## The provider block

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "wg-gateway": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Workgraph staging oss gateway",
      "options": {
        "baseURL": "https://gateway.ai.cloudflare.com/v1/<ACCOUNT_ID>/workgraph-staging-oss/compat",
        "headers": {
          "cf-aig-authorization": "Bearer <WG_AI_GATEWAY_TOKEN>",
          "cf-aig-metadata": "{\"role\":\"polecat\",\"cell\":\"oss\",\"bead\":\"wg-uhj\",\"rig\":\"spike\"}"
        }
      },
      "models": {
        "workers-ai/@cf/moonshotai/kimi-k2.7-code": {
          "name": "Kimi K2.7 Code",
          "limit": { "context": 262144, "output": 8192 }
        }
      }
    }
  }
}
```

```bash
opencode run -m "wg-gateway/workers-ai/@cf/moonshotai/kimi-k2.7-code" "Reply with exactly: OK"
```

Four things to know before copying it.

**`options.headers` is the whole mechanism.** It is what the Claude Code path spends
`ANTHROPIC_CUSTOM_HEADERS` on, and it survives to the gateway. Both headers must travel
together — the credential and the attribution are one variable, for the same reason the
runner's `renderHeaders` says so.

**`limit` is required, not optional.** Without it OpenCode requests `max_tokens=32000`,
and a model that caps lower rejects the request before doing anything:
`max_tokens=32000 cannot be greater than max_model_len=max_total_tokens=24000`.

**The model name carries the provider twice.** `-m` takes
`<opencode-provider>/<gateway-provider>/<model>` — here
`wg-gateway/workers-ai/@cf/moonshotai/kimi-k2.7-code`. The first segment names the block
above; the rest is what the gateway's `/compat` endpoint expects.

**The credential belongs outside the working directory.** This spike wrote it into a
scratch dir, which is fine for a spike and wrong for a run: an agent with `Read` in its
own working directory can read its own gateway token, which is a mistake this repository
has already made once and fixed by moving the settings file out.

## What the account can reach

Probed through the staging `oss` gateway on the day:

| Model | | Note |
|---|---|---|
| `@cf/moonshotai/kimi-k2.7-code` | 200 | 262k context; the one used for the run |
| `@cf/moonshotai/kimi-k2.6` | 200 | |
| `@cf/deepseek-ai/deepseek-v4-flash-0731` | 200 | |
| `@cf/zai-org/glm-5.3-flash` | 200 | |
| `@cf/qwen/qwen3-30b-a3b-fp8` | 200 | |
| `@cf/google/gemma-4-26b-a4b-it` | 200 | |
| `@cf/moonshotai/kimi-k3` | **403** | "Account 3218567 is not allowed to access" |

K3 is the one model gated. Cloudflare documents prepaid credits as granting access to
Workers AI models that otherwise need the Workers Paid plan, so Unified Billing is the
expected fix — but that is a claim from documentation, not something this spike proved.
Re-run the probe after credits are loaded.

## Two findings that change the next bead

**Context window filters models before quality does.** OpenCode's agent system prompt is
about 19,900 tokens. `llama-3.3-70b-instruct-fp8-fast` has a 24k context and fails with
"your prompt contains at least 19905 input tokens" before doing any work. It is a good T0
classifier and an unusable worker; those are unrelated facts. `wg-hvb` should screen on
context first, then on tool-use, then on quality — in that order, because each is cheaper
than the next.

**A 20k-token floor changes the cost arithmetic.** "Reply with exactly: OK" cost 1.82
cents, because the prompt goes in whole on every request whatever the task. Cheap
per-token rates do not make cheap runs when the floor is that high. Compare cost per
completed bead, not per token, and measure Claude Code's prompt on the same task rather
than assuming it is smaller.
