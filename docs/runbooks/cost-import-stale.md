# Runbook: spend is no longer being imported

**Alert rule:** `platform_cost_import_stale` · **Raised by:** `wg-monitor` on the control node

`wg-costimport` has not completed a pass for at least one AI Gateway within the
declared window. The inbox item names each gateway and says whether it is
**stale** (imported once, too long ago), **never** (no import on record), or
**incomplete** (running, but not caught up).

## What this is measuring, and what it is not

The age of the last successful **import run**, not the age of the newest usage
record.

Those come apart on exactly the environment where it matters least to be woken:
a gateway nobody has used for a week has a newest record a week old however
punctually the importer ran. Measuring the record alerts loudest on the quietest
system. The budget check made this mistake and refused every dispatch on staging
with `spend data is 216h49m old` while the importer was running perfectly every
hour (wg-7jz, 0033).

## Why it matters even though nothing is on fire

Everything built on the spend data degrades quietly rather than failing:

- the budget gate refuses dispatches rather than guessing, so **work stops** with
  a reason that sounds like a budget problem and is not;
- cost reports are confidently wrong, because the rows are simply absent rather
  than marked missing;
- and the AI Gateway log rotates on `DELETE_OLDEST`, so spend not imported before
  the window passes is **gone** — not late, gone.

That last one is why this is worth acting on the same day rather than the same
week.

## 1. Is the timer running

```bash
systemctl status wg-costimport.timer --no-pager
systemctl status wg-costimport.service --no-pager
journalctl -u wg-costimport.service -n 50 --no-pager
```

The service is a `oneshot` driven by the timer, so `inactive (dead)` is its
normal resting state. The timer is the thing that must be `active`.

## 2. Run it by hand and read the summary

```bash
systemctl start wg-costimport.service
journalctl -u wg-costimport.service -n 20 --no-pager
```

Each line reports one gateway:

```
workgraph-staging-oss  from=... fetched=394 imported=0 duplicate=394 complete=true
```

`complete=false` is the **incomplete** state: the pass stopped at its per-run cap
rather than reaching the start of its window. It will catch up over successive
runs, and a budget decision taken before it does is resting on a partial read.

## 3. The usual causes

**The token.** The importer needs a Cloudflare token scoped to *AI Gateway:
Read*, delivered as a systemd credential. If it was rotated or revoked, the run
fails with HTTP 403 and the log says so. The token is recorded in
`credential_registry` as `cost_import_cf_token`.

```bash
sudo -u postgres psql -d workgraph -c \
  "SELECT name, rotated_at, rotate_every FROM credential_registry WHERE name = 'cost_import_cf_token'"
```

**A renamed or deleted gateway.** The gateway list is declared in
`costimport_gateways`. A gateway that no longer exists fails every run and will
never recover on its own — remove it from the declaration in the same change
that removes the gateway.

**The database.** The importer writes through `system_record_usage` and exits
non-zero if it cannot reach PostgreSQL. `platform_api_unavailable` will usually
have fired first.

## 4. Confirm it recovered

```bash
sudo -u postgres psql -d workgraph -c \
  "SELECT gateway, ran_at, entries_seen, complete FROM usage_import_runs ORDER BY gateway"
```

Every declared gateway should have a `ran_at` within the window. The alert
clears on the next monitor pass, five minutes later.
