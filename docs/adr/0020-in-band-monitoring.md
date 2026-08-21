# ADR-0020: platform monitoring is in-band, deterministic, and knows its blind spot

**Status:** accepted · **Date:** 2026-08-21 · **Work package:** WP-I1

## Context

The platform had no monitoring of any kind. The threat model named detection as
the weakest link, and it named it from experience: a deployment silently reset the
database password, reconciliation failed authentication on every attempt, the
webhook backlog stopped draining, and nobody noticed for two days until somebody
asked why a view looked stale. Every component behaved exactly as written. What
was missing was anything whose job was to look.

WP-I1 requires that failures of the API, webhooks, agent stall, disk and backup
each trigger the correct alert, and that the alert reaches the designated operator
with a runbook link.

## Decision

**Alerts are delivered into the existing attention inbox, not to a new channel.**
The inbox already resolves recipients, deduplicates, ranks against everything else
competing for attention, and supports snoozing. A separate alerting channel would
have meant a second place to look and a second thing to keep working.

**The decision layer is a pure function.** Gathering (a socket, three queries, a
statfs, a file read) is separated from judging, exactly as the deterministic
witness does it (ADR-0019). The interesting cases — a queue that is deep but
fresh, a filesystem that cannot be measured, a backup that has never run — are
painful to arrange against a live system and trivial as table-driven tests.

**No model is involved.** An alerting path that costs money per evaluation is one
somebody eventually turns off, and a non-deterministic one cannot be tested.

**A systemd timer, not a loop.** `systemctl list-timers` says when it last ran and
when it runs next, a crash restarts without supervision logic here, and a missed
run is recorded by the operating system rather than being something the monitor
would have to notice about itself.

**Thresholds are flags.** A threshold that needs a release to change is one that
gets worked around, and the acceptance test needs to induce each failure without
filling a real disk.

## Consequences, including one that is not fixed

Three decisions here are about failures that look like successes, which is the
category that made this work package necessary:

- **"I could not look" is a failing finding, never a silent pass.** A check whose
  gathering failed reports as failing. Collapsing it into "healthy" is how a check
  stops running without anyone noticing.
- **An alert that reached nobody makes the unit fail.** `system_raise_platform_alert`
  returns the number of inboxes reached, and zero exits non-zero, because an
  undelivered alert is indistinguishable from a handled one.
- **Backup obligations are declared, not discovered.** A check that reports on the
  backups it can find treats the total absence of a backup as nothing to say.
  `postgres` is declared before WP-I2 implements it, so the gap sits in an
  operator's inbox instead of being discovered during a restore.

Building this surfaced a bug of exactly the kind it exists to catch. The monitor's
first version queried `github_deliveries` and `agent_health_events` directly. Both
have row-level security enabled with **no policy at all** — deliberately, since
they hold raw payloads and infrastructure facts no user session should read — so
the application role sees an empty table rather than a filtered one. The webhook
check would have reported "0 deliveries unprocessed" for ever, which reads as
healthy. Migration 0014 records the same mistake being made before it. The fix is
narrow `SECURITY DEFINER` functions returning **aggregates only**: a count and two
timestamps, so the monitor cannot become a way to read restricted repository
activity. The acceptance test caught this independently, by running a binary built
before the fix and watching it miss a delivery that had been stuck for two hours.

### The blind spot

This runs **on** the node and reports **through** the database. If the node is
down or PostgreSQL is unreachable, it cannot deliver an alert about that — the
case where alerting matters most is the one an in-band monitor cannot cover.

It degrades as well as it can: the checks that need no database still run, each
failing one is written to the journal prefixed `ALERT-UNDELIVERABLE` with its
runbook, and the unit exits non-zero so it shows in `systemctl list-units
--failed`. Nothing off the node watches either of those, so **a dead node is
currently silent.** That is stated here rather than left for someone to discover.

Closing it needs a prober outside the node, and the obvious version is theatre: a
Cloudflare Health Check against the public hostname receives an Access login
redirect from the edge without ever reaching the origin, so it would report
healthy with the machine switched off. The real fix is an Access policy admitting
a service token on `/health/ready` only, plus a scheduled prober that presents the
token — tracked separately.

## Alternatives considered

**Prometheus and Alertmanager.** The standard answer, and too much machinery for
two nodes: a metrics store, a rules language, a notification router and a
dashboard stack, all of which then need monitoring themselves. The five checks
required here are three queries, an HTTP request, a statfs and a file read.

**A hosted monitoring service.** Solves the blind spot properly and adds a vendor,
a credential, and per-node cost, for a platform whose entire alerting audience is
currently one organisation admin. Worth revisiting when the node count justifies
it.

**Invent a project to hang platform alerts on.** The first attempt, abandoned when
the schema objected: projects require an organisation, a primary owner and a
backup owner that differ, because full-cycle ownership must not be a single point
of failure. Satisfying those columns would have meant a migration silently
inventing an owner for the platform. `attention_items.project_id` is nullable, so a
platform alert simply has no project — which is the truth.
