#!/usr/bin/env python3
"""Structural guards on the infrastructure configuration.

These assert the security posture that the whole origin design depends on. They
are cheap, they run before any apply, and each one encodes a mistake that would
be easy to make and expensive to discover in production.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
MODULE = ROOT / "infra" / "tofu" / "modules" / "environment"

problems = []


def block(text: str, name: str) -> str | None:
    """Return the body of `variable "<name>" { ... }`, brace-matched."""
    m = re.search(r'variable\s+"%s"\s*\{' % re.escape(name), text)
    if not m:
        return None
    depth, start = 0, m.end() - 1
    for i in range(start, len(text)):
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
            if depth == 0:
                return text[start + 1 : i]
    return None


def check_no_inbound_rules() -> None:
    """The origin must expose no inbound service.

    Hetzner firewalls are allow-lists, so the absence of inbound rules drops
    everything. Exactly one inbound rule is tolerated: the break-glass SSH rule,
    which is itself gated on a variable that defaults to empty.
    """
    main = (MODULE / "main.tf").read_text()
    inbound = re.findall(r'direction\s*=\s*"in"', main)
    if len(inbound) > 1:
        problems.append(
            f"{len(inbound)} inbound firewall rules found; the origin is reached "
            "only through Cloudflare Tunnel and must expose no inbound service"
        )


def check_ssh_closed_by_default() -> None:
    """A default that opens SSH to the internet would be catastrophic."""
    variables = (MODULE / "variables.tf").read_text()
    body = block(variables, "admin_ssh_cidrs")
    if body is None:
        problems.append("admin_ssh_cidrs variable is missing")
        return
    m = re.search(r"default\s*=\s*(\[[^\]]*\])", body)
    if not m:
        problems.append("admin_ssh_cidrs has no explicit default; it must default to []")
    elif m.group(1).replace(" ", "") != "[]":
        problems.append(
            f"admin_ssh_cidrs must default to [] so no public SSH port is opened, got {m.group(1)}"
        )


def check_nodes_bootstrap_without_inbound() -> None:
    """Every node must bootstrap itself; nothing may depend on reaching it.

    The firewall has no inbound rules, so a node that does not configure its own
    outbound tunnel at first boot is unreachable and unrecoverable without
    opening a port. prevent_destroy is deliberately NOT asserted here: it
    contradicts the WP-B1 acceptance criterion that staging is destroyed and
    recreated from code, and destroy is gated by two approvers in policy instead.
    """
    main = (MODULE / "main.tf").read_text()
    servers = re.findall(r'resource\s+"hcloud_server"\s+"(\w+)"', main)
    for name in servers:
        m = re.search(
            r'resource\s+"hcloud_server"\s+"%s"\s*\{(.*?)\n\}' % re.escape(name), main, re.S
        )
        if not m or "user_data" not in m.group(1):
            problems.append(
                f'hcloud_server "{name}" has no user_data. With no inbound firewall rule, a node '
                "that does not dial out at first boot cannot be reached at all."
            )


def check_cloudinit_template_escaping() -> None:
    """Catch templatefile over-escaping in cloud-init.

    templatefile only treats `${` specially. `$(` and `$((` are ordinary shell
    and must be written plainly; `$$(` renders literally, and `$$` is the
    shell's PID, so the script dies with a syntax error. That failure happens on
    a host with no inbound path, where there is nothing to read the error from.
    """
    for tpl in sorted((MODULE / "templates").glob("*.tftpl")):
        for i, line in enumerate(tpl.read_text().splitlines(), 1):
            # A comment mentioning the sequence is harmless; only executable
            # lines matter.
            if line.lstrip().startswith("#"):
                continue
            if "$$(" in line:
                problems.append(
                    f"{tpl.relative_to(ROOT)}:{i}: '$$(' renders literally. "
                    "Write $( for command substitution and $(( for arithmetic; "
                    "only ${ needs escaping as $${."
                )


# DNS records this configuration is allowed to manage. Adding a name here is a
# deliberate act; a record not listed is either a mistake or someone else's.
ALLOWED_DNS_RECORDS = {"app", "ssh", "ssh_execution", "api"}


def check_dns_records_are_declared() -> None:
    """Only manage records we have explicitly declared, and never a wildcard.

    The Workgraph zone is ours, but the deploy token holds DNS write on a whole
    zone, and a wildcard or a zone-level resource turns a narrow permission into
    a broad one. This keeps the blast radius equal to the records we name.
    """
    main = (MODULE / "main.tf").read_text()

    records = set(re.findall(r'resource\s+"cloudflare_dns_record"\s+"(\w+)"', main))
    unexpected = records - ALLOWED_DNS_RECORDS
    if unexpected:
        problems.append(
            f"undeclared cloudflare_dns_record resources: {sorted(unexpected)}. "
            "Add the name to ALLOWED_DNS_RECORDS only after deciding it should exist."
        )

    # A wildcard record would capture every unclaimed name in the zone.
    for m in re.finditer(r'resource\s+"cloudflare_dns_record"\s+"\w+"\s*\{(.*?)\n\}', main, re.S):
        if re.search(r'name\s*=\s*"[^"]*\*', m.group(1)):
            problems.append("a wildcard DNS record would capture every unclaimed name in the zone")

    if re.search(r'resource\s+"cloudflare_zone"', main):
        problems.append("this configuration must not manage the zone itself, only records in it")


def check_tfvars_hold_no_secrets() -> None:
    """Committed tfvars carry environment config, never credentials.

    Account and zone identifiers are not secrets. A token, key, or passphrase
    would be, and committing one is the failure this repository is built to
    prevent.
    """
    import re as _re

    # Note the deliberate absence of \b before the keyword: "_" is a word
    # character, so \bsecret\b does NOT match inside "client_secret". That gap
    # would have missed the very next credential added to this repository.
    secretish = _re.compile(
        r"(?i)[a-z0-9_]*"
        r"(token|secret|password|passphrase|api_key|apikey|private_key|credential)"
        r"[a-z0-9_]*\s*=|"
        r"(gh[pous]_|github_pat_|sk-|AKIA|AIza|cfat_|GOCSPX-|-----BEGIN)"
    )
    for path in sorted((ROOT / "infra" / "tofu").rglob("*.tfvars")):
        for i, line in enumerate(path.read_text().splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue
            if secretish.search(line):
                rel = path.relative_to(ROOT)
                problems.append(
                    f"{rel}:{i}: committed tfvars must not contain a credential"
                )


# Attributes we have deliberately reviewed as safe to set on the SHARED account.
# Workgraph does not own this Cloudflare account: it carries ~50 Datopian
# production zones and other teams' tunnels. An account-level setting is
# therefore production-affecting whatever its name suggests.
#
# Adding a name here is a claim that you have established what the setting does
# to traffic that is not ours.
REVIEWED_ACCOUNT_ATTRS = {
    "account_id",
    "auth_domain",
    "name",
    "session_duration",
    "user_seat_expiration_inactive_time",
    "auto_redirect_to_identity",
}

# Settings that may appear only at a specific value. Declaring the safe value
# explicitly is better than omitting the attribute: Terraform then reverts it if
# someone flips it in the dashboard, instead of ignoring the change.
PINNED_ACCOUNT_ATTRS = {
    # Set true on 2026-08-12; returned 403 across live zones until reverted
    # (wg-8yv.47). Pinned false so a dashboard change is undone on next apply.
    "deny_unmatched_requests": "false",
}


def check_account_level_settings() -> None:
    """Fail on an unreviewed attribute of the shared-account Zero Trust org."""
    path = ROOT / "infra" / "tofu" / "account" / "main.tf"
    if not path.exists():
        return
    text = path.read_text()

    m = re.search(
        r'resource\s+"cloudflare_zero_trust_organization"\s+"\w+"\s*\{(.*?)\n\}', text, re.S
    )
    if not m:
        return

    for line in m.group(1).splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        attr = re.match(r"([a-z_]+)\s*=\s*(\S+)", stripped)
        if not attr:
            continue
        name, value = attr.group(1), attr.group(2).rstrip(",")

        if name in PINNED_ACCOUNT_ATTRS:
            expected = PINNED_ACCOUNT_ATTRS[name]
            if value != expected:
                problems.append(
                    f"infra/tofu/account/main.tf: '{name}' must stay {expected} on this shared "
                    f"account, got {value}. It broke live production zones once (wg-8yv.47)."
                )
        elif name not in REVIEWED_ACCOUNT_ATTRS:
            problems.append(
                f"infra/tofu/account/main.tf: '{name}' is an unreviewed account-level "
                "setting on a Cloudflare account shared with Datopian production. Establish what "
                "it does to traffic that is not ours, then add it to REVIEWED_ACCOUNT_ATTRS."
            )


def check_spend_limit_not_in_terraform() -> None:
    """The spend limit must stay out of the Terraform resource.

    The provider serialises the rule field as limit_type where the API requires
    limitType, so declaring it makes every apply fail — and the create path
    reports success while storing nothing, which is how a $100 ceiling came to
    exist in state and not on the gateway.

    It is applied and verified by scripts/ai_gateway_spend_limits.py instead.
    This guard exists because re-adding the attribute looks like an obvious
    improvement to anyone who has not hit the failure.
    """
    path = MODULE / "ai_gateway.tf"
    if not path.exists():
        return
    text = path.read_text()

    body = re.search(r'resource\s+"cloudflare_ai_gateway".*?\n\}', text, re.S)
    if not body:
        return
    declared = re.search(r"^\s*spend_limits\s*=", body.group(0), re.M)
    if declared:
        problems.append(
            "modules/environment/ai_gateway.tf declares spend_limits. The provider sends "
            "limit_type where the API requires limitType, so the apply fails and a create "
            "silently stores nothing. Use scripts/ai_gateway_spend_limits.py."
        )
    if "ignore_changes" not in body.group(0) or "spend_limits" not in body.group(0):
        problems.append(
            "modules/environment/ai_gateway.tf must ignore_changes on spend_limits, or a "
            "successful apply will propose 'limit -> null' and silently remove the budget."
        )
    if "authentication = true" not in text:
        problems.append(
            "every AI Gateway must set authentication = true; without it the gateway URL "
            "alone is enough to spend money, and that URL travels in environment variables."
        )


def main() -> int:
    for check in (
        check_no_inbound_rules,
        check_ssh_closed_by_default,
        check_nodes_bootstrap_without_inbound,
        check_cloudinit_template_escaping,
        check_dns_records_are_declared,
        check_tfvars_hold_no_secrets,
        check_account_level_settings,
        check_spend_limit_not_in_terraform,
    ):
        check()

    if problems:
        print("infrastructure checks FAILED:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("infrastructure checks OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
