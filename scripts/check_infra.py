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


# The Access bypass policies that are allowed to exist, and why each one is not
# a hole. A bypass removes the outer gate entirely for whatever application
# carries it, so ADDING A NAME HERE IS A SECURITY DECISION, not configuration:
# it is a claim that something else authenticates the traffic, and the
# reviewer's job is to ask what.
#
#   webhook_bypass      GitHub cannot complete an Access challenge. The origin
#                       verifies X-Hub-Signature-256 with a constant-time
#                       compare before parsing anything (plan section 9.3).
#                       Here the bypass IS the boundary.
#   pubsub_push_bypass  Google Pub/Sub cannot either. It attaches an OIDC token
#                       that the receiver verifies against Google's public keys,
#                       the issuer, and an audience bound to this exact endpoint
#                       (ADR-0026). The node holds no secret that could forge a
#                       delivery.
#   api_token_bypass    A tool holding a personal API token, on its own hostname
#                       so the exception never reaches the human interface
#                       (wg-p4h.1).
ALLOWED_BYPASS_POLICIES = {"webhook_bypass", "pubsub_push_bypass", "api_token_bypass"}

# Applications where a bypass legitimately covers a whole host rather than one
# path, because the host exists for exactly that purpose.
BYPASS_WHOLE_HOST_OK = {"api"}


def _resource_bodies(text: str, kind: str):
    r"""Yield (name, body) for each resource of `kind`, brace-matched.

    Brace-matched rather than a lazy `.*?\n\}`: a policy body contains nested
    blocks (include, require), so the non-greedy form stops at the first nested
    close and misses anything declared after it -- including a `decision` line.
    That is not a theoretical difference; it is the whole reason a bypass could
    hide from a simpler check.
    """
    for m in re.finditer(r'resource\s+"%s"\s+"(\w+)"\s*\{' % re.escape(kind), text):
        depth, start = 0, m.end() - 1
        for i in range(start, len(text)):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    yield m.group(1), text[start + 1 : i]
                    break


def check_access_bypasses_are_declared() -> None:
    """Only bypass Access where something else authenticates, and never broadly.

    The DNS guard above exists because the deploy token can write any record in
    the zone. This is the same argument applied to the larger of the two powers:
    a `cloudflare_zero_trust_access_policy` with decision = "bypass" removes the
    outer gate for its application, it is a few lines of HCL, and it reads like
    configuration rather than like a decision.

    Not hypothetical. When this check was proposed there were two bypasses and
    the bead predicted "a third would pass CI silently today". A third --
    pubsub_push_bypass, for the Google Workspace event receiver -- was added on
    2026-08-31 and CI said nothing. That is exactly the gap.
    """
    main = (MODULE / "main.tf").read_text()

    bypass_policies = {
        name for name, body in _resource_bodies(main, "cloudflare_zero_trust_access_policy")
        if re.search(r'decision\s*=\s*"bypass"', body)
    }

    unexpected = bypass_policies - ALLOWED_BYPASS_POLICIES
    if unexpected:
        problems.append(
            f"undeclared Access bypass policies: {sorted(unexpected)}. A bypass removes "
            "the outer gate for its application; add the name to ALLOWED_BYPASS_POLICIES "
            "only after deciding what else authenticates that traffic."
        )

    # A name listed but absent means the list has drifted from the module, and a
    # stale entry silently pre-authorises whatever takes that name next.
    missing = ALLOWED_BYPASS_POLICIES - bypass_policies
    if missing:
        problems.append(
            f"ALLOWED_BYPASS_POLICIES names policies that do not exist: {sorted(missing)}. "
            "Remove them, or the list pre-authorises whatever takes the name next."
        )

    # An application carrying a bypass must be narrow. On a wildcard domain the
    # exception extends to every name the wildcard matches, which is how one
    # unauthenticated endpoint becomes an unauthenticated site.
    for app, body in _resource_bodies(main, "cloudflare_zero_trust_access_application"):
        carries = [p for p in bypass_policies
                   if re.search(r"cloudflare_zero_trust_access_policy\.%s\b" % re.escape(p), body)]
        if not carries:
            continue
        dom = re.search(r'domain\s*=\s*"([^"]*)"', body)
        if not dom:
            continue
        domain = dom.group(1)
        if "*" in domain:
            problems.append(
                f"Access application {app!r} carries a bypass on wildcard domain "
                f"{domain!r}; the exception would extend to every name it matches"
            )
        elif "/" not in domain and app not in BYPASS_WHOLE_HOST_OK:
            problems.append(
                f"Access application {app!r} carries a bypass over a whole host "
                f"({domain!r}) rather than one path. Scope it to a path, or record "
                "the host in BYPASS_WHOLE_HOST_OK with the reason it exists."
            )


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


def check_service_required_vars_are_not_env_lookups() -> None:
    """A value a service needs to start must not come from the environment.

    control_api_google_subject was `lookup('env', 'WG_GOOGLE_SUBJECT')`. Nothing
    set it, so it resolved to the empty string, the unit templated with an empty
    subject, and workgraph-workspace failed hourly on "no delegation subject"
    for as long as nobody read the journal (wg-bil). The env lookup is what made
    that silent: an absent variable and an absent value look identical, and
    neither shows up in review.

    Ansible has no way to distinguish "not set" from "set to empty" here, so the
    guard is structural: these variables are literals in Git, where a missing one
    is a diff. It deliberately does not check the *value* — that would just be
    the address written twice.
    """
    path = ROOT / "infra" / "ansible" / "group_vars" / "all" / "secrets.yml"
    if not path.exists():
        problems.append(f"{path} is missing, so its variables cannot be checked")
        return
    text = path.read_text()
    # Not every variable here: a genuine secret SHOULD come from the environment,
    # which is the whole point of with_secrets.sh. Only the ones the file itself
    # says are not secrets, and that a unit refuses to start without.
    for name in ("control_api_google_subject", "control_api_google_topic"):
        m = re.search(r"^%s:\s*(.+)$" % re.escape(name), text, re.M)
        if not m:
            problems.append(f"{name} is not set in {path.name}")
            continue
        value = m.group(1).strip()
        if "lookup(" in value or "env" == value.strip("\"'{} "):
            problems.append(
                f"{name} reads from the environment; it is not a secret and a "
                f"service will not start without it, so it belongs in Git as a "
                f"literal where an absent value is visible in review"
            )
        if value.strip('"' + "'") == "":
            problems.append(f"{name} is set to an empty value, which is how wg-bil happened")


def check_every_binary_install_carries_the_code_tag() -> None:
    """A task that installs a node binary must be selectable by --tags code.

    The tag exists so a code-only deploy touches about a dozen tasks instead of
    168. That is only safe while the tag is complete: a new binary added without
    it would be silently skipped by exactly the deploy meant to ship it, and the
    run would report success while the old process kept serving. The failure is
    invisible, which is why it is a check rather than a note in the README.

    Recognising a binary task by a `*_local*binary` variable used as the copy
    `src:`, rather than by task name, because names get rewritten and this should
    still hold afterwards. It has to be the src specifically: several unit-file
    templates mention the same variable in a `when:` guard, and those are
    configuration rather than code — a changed unit belongs to a full run.
    """
    roles = ROOT / "infra" / "ansible" / "roles"
    if not roles.is_dir():
        problems.append(f"{roles} is missing, so the deploy tags cannot be checked")
        return
    for tasks in sorted(roles.glob("*/tasks/main.yml")):
        text = tasks.read_text()
        # Split on task boundaries: a line beginning "- " at column zero.
        blocks = re.split(r"(?m)^(?=-\s)", text)
        for b in blocks:
            if not re.search(r"(?m)^\s+src:\s*[\"']?\{\{\s*\w*_local\w*binary", b):
                continue
            name = re.match(r"-\s+name:\s*(.+)", b)
            label = name.group(1).strip() if name else b.splitlines()[0][:60]
            if not re.search(r"(?m)^\s+tags:.*\bbinaries\b", b):
                problems.append(
                    f"{tasks.parent.parent.name}: task '{label}' installs a binary but "
                    f"carries no `binaries` tag, so --tags code would skip it and "
                    f"deploy stale code while reporting success"
                )


def check_example_env_is_complete_and_not_ours() -> None:
    """infra/tofu/envs/example must document every knob, and hold none of ours.

    The example is the only worked configuration an outside deployment has. Two
    ways it rots, both silent:

    A variable is added to the environment and not to the example, so somebody
    installing reads a file that no longer describes what they must set.

    A value of OURS survives in it. That already happened: the copy inherited
    `ai_gateway_store_id`'s default, which is Datopian's AI Gateway log store,
    so a third party's plan would have pointed at our resource. A default is
    the easiest kind of leftover to miss, because nothing about the file looks
    filled in.
    """
    env = ROOT / "infra" / "tofu" / "envs" / "example"
    if not env.is_dir():
        problems.append("infra/tofu/envs/example is missing; an outside "
                        "deployment has no worked configuration to copy")
        return

    variables = (env / "variables.tf")
    tfvars = (env / "terraform.tfvars.example")
    for f in (variables, tfvars, env / "backend.hcl.example", env / "main.tf"):
        if not f.exists():
            problems.append(f"{f.relative_to(ROOT)} is missing from the example environment")
            return

    declared = set(re.findall(r'variable\s+"([^"]+)"', variables.read_text()))
    given = set(re.findall(r"^([a-z_]+)\s*=", tfvars.read_text(), re.M))
    for name in sorted(declared - given):
        problems.append(
            f"{name} is declared in envs/example/variables.tf but not in "
            f"terraform.tfvars.example; somebody copying the example would not "
            f"know they can set it")
    for name in sorted(given - declared):
        problems.append(
            f"{name} is set in envs/example/terraform.tfvars.example but is not "
            f"a variable there; it would be ignored")

    # A real terraform.tfvars in the example directory would be applied by
    # accident, and would be somebody's real configuration in a public repo.
    if (env / "terraform.tfvars").exists():
        problems.append(
            "infra/tofu/envs/example/terraform.tfvars exists; the example must "
            "carry only terraform.tfvars.example so `tofu apply` there refuses")

    # CONFIGURATION only. The README in this directory names us on purpose --
    # "infra/secrets/*.enc.yaml are encrypted to Datopian's keys, create your
    # own" is the sentence that stops somebody trying to use them, and a check
    # that forbade it would delete the explanation to protect the example from
    # a value it does not contain.
    ours = ("datopian", "openbases.com", "10ba352dd98c4f2db387148e7313e451")
    config = (".tf", ".tfvars", ".example", ".hcl")
    for f in sorted(env.iterdir()):
        if not f.is_file() or not f.name.endswith(config):
            continue
        text = f.read_text()
        for needle in ours:
            for line in text.splitlines():
                if needle in line.lower() and not line.lstrip().startswith("#"):
                    problems.append(
                        f"{f.relative_to(ROOT)} still carries one of our values "
                        f"({needle!r}) outside a comment: {line.strip()[:80]}")


def check_every_installed_binary_has_a_source() -> None:
    """A binary install guarded on an unset variable never runs.

    The pattern in control_api and its siblings is: `copy` the binary, with
    `when: <var> | length > 0` so a machine without a build does not fail. The
    variable is meant to be assigned in group_vars/all/binaries.yml with a
    first_found lookup into the build directory.

    control_api_local_worker_binary was never assigned. The default was "", the
    guard was therefore always false, the task skipped on every deploy and
    reported ok, and the worker binary on staging was from 22 August -- so every
    change to cmd/worker and internal/reconcile for two and a half weeks was
    merged, deployed and never running. Nothing failed, which is what made it
    survive.

    So: every variable used as the source of a binary install must have a real
    assignment, not just an empty default.
    """
    binaries = ROOT / "infra" / "ansible" / "group_vars" / "all" / "binaries.yml"
    if not binaries.exists():
        problems.append("infra/ansible/group_vars/all/binaries.yml is missing")
        return
    assigned = set(re.findall(r"^([a-z_]+):\s*\S", binaries.read_text(), re.M))

    roles = ROOT / "infra" / "ansible" / "roles"
    for task_file in sorted(roles.glob("*/tasks/*.yml")):
        text = task_file.read_text()
        # `src: "{{ some_local_binary }}"` -- the shape every binary install uses.
        for var in re.findall(r'src:\s*"\{\{\s*([a-z_]*local[a-z_]*binary)\s*\}\}"', text):
            if var not in assigned:
                problems.append(
                    f"{task_file.relative_to(ROOT)} installs a binary from {var}, "
                    f"which group_vars/all/binaries.yml never assigns. Its default "
                    f"is empty, so the install is guarded off and skips silently on "
                    f"every deploy while reporting ok")


def check_restart_handlers_can_fire_on_a_tagged_deploy() -> None:
    """A handler guarded on an untagged fact never fires on a tagged deploy.

    The pattern: a copy task notifies "Restart the X", and the handler guards on
    `X_stat.stat.exists` so it does not start a unit whose ExecStart is missing
    -- which produces a restart loop that looks exactly like a crashing service.
    The guard is right. What was wrong is that the stat registering that fact
    carried no tags, so on `ansible-playbook --tags code` it did not run, the
    variable was undefined, `| default(false)` made the guard false, and the
    handler skipped while reporting ok in 0.04s.

    The worker on staging had been running since 26 August because of it: its
    binary was replaced on later deploys and the process never restarted, so the
    running code had no relation to the deployed code. The monitor had the same
    shape.

    So: if a handler guards on a registered fact, the task registering it must
    carry at least the tags of the task that notifies the handler.
    """
    roles = ROOT / "infra" / "ansible" / "roles"
    for handlers in sorted(roles.glob("*/handlers/main.yml")):
        role = handlers.parent.parent
        tasks_dir = role / "tasks"
        if not tasks_dir.is_dir():
            continue
        tasks = "".join(p.read_text() for p in sorted(tasks_dir.glob("*.yml")))

        for var in set(re.findall(r"when:\s*([a-z_]+)\.stat\.exists", handlers.read_text())):
            # The task that registers the fact, and whether it is tagged.
            block = re.search(
                r"- name: ([^\n]+)\n((?:(?!\n- name:).)*?register:\s*%s\b(?:(?!\n- name:).)*)" % var,
                tasks, re.S)
            if not block:
                problems.append(
                    f"{role.name}: a handler guards on {var}.stat.exists, but no task "
                    f"registers {var}, so the guard is always false and the handler "
                    f"never fires")
                continue
            if "tags:" not in block.group(2):
                problems.append(
                    f"{role.name}/tasks: {block.group(1)!r} registers {var}, which a "
                    f"restart handler guards on, but carries no tags. On "
                    f"`--tags code` it does not run, the guard is false, and the "
                    f"service is never restarted while the handler reports ok -- the "
                    f"deployed binary changes and the running process does not")


def check_a_job_deadline_stays_under_the_cell_ceiling() -> None:
    """The dispatcher's per-job deadline must be below the cell's backstop.

    Two limits end a long run, and they mean different things.
    `dispatcher_deadline` is the job's own budget, and hitting it is a normal
    outcome the dispatcher records against the bead. `agent_max_runtime_minutes`
    is the cell's backstop for an agent nobody is watching, enforced by the
    reaper.

    Ordered, the job's budget expires first and the run is recorded as having
    run out of time. Inverted -- or equal -- the backstop is what ends every
    long run, and it reports a reaped process rather than a bead that needed
    longer. The dispatcher's number was raised from 15m to 30m on 2026-09-09
    when agents got a shell, which is what made this worth asserting: the
    ordering was previously true by a margin nobody was going to close by
    accident, and it is now a deliberate 30-against-45.
    """
    defaults = ROOT / "infra" / "ansible" / "roles" / "execution_cell" / "defaults" / "main.yml"
    text = defaults.read_text()

    deadline = re.search(r"^dispatcher_deadline:\s*(\d+)([smh])\s*$", text, re.M)
    ceiling = re.search(r"^agent_max_runtime_minutes:\s*(\d+)\s*$", text, re.M)
    if not deadline or not ceiling:
        problems.append(
            "execution_cell/defaults: dispatcher_deadline or "
            "agent_max_runtime_minutes is missing or not a plain value, so the "
            "ordering between the job budget and the cell backstop cannot be "
            "checked")
        return

    unit = {"s": 1 / 60, "m": 1, "h": 60}[deadline.group(2)]
    minutes = int(deadline.group(1)) * unit
    cap = int(ceiling.group(1))
    if minutes >= cap:
        problems.append(
            f"execution_cell/defaults: dispatcher_deadline is {deadline.group(0).split(':')[1].strip()} "
            f"({minutes:g}m) and agent_max_runtime_minutes is {cap}, so the cell's "
            f"backstop fires first or at the same moment. Every long run would be "
            f"reported as a reaped process rather than a job that ran out of its "
            f"own budget. Keep the job deadline below the ceiling.")


def check_every_ansible_secret_is_exported_by_the_deploy() -> None:
    """A variable Ansible reads from the environment must be exported by the
    wrapper that runs the deploy.

    with_secrets.sh decrypts the SOPS file and maps its keys onto environment
    variables BY HAND -- written out rather than derived, so that a rename is a
    visible edit. The cost of that choice is this failure: add a key to the
    encrypted file and a `lookup('env', ...)` in group_vars, forget the export,
    and the lookup resolves to "" forever. Every task guarded on it skips,
    reports ok, and the credential is silently never installed.

    That is precisely what happened to PORTALJS_TOKEN, and it would have been
    found by an operator wondering why their token did nothing rather than by
    the deploy. The same shape is recorded twice more in this repository: the
    worker binary that was never wired to a variable and left staging three
    weeks stale, and WG_GOOGLE_SUBJECT, which resolved to "" until the
    reconciler failed hourly on "no delegation subject".

    Comments are stripped before matching. The note explaining that last
    incident still names WG_GOOGLE_SUBJECT, and a check that reads prose cries
    wolf about a variable deliberately replaced by a literal.
    """
    group_vars = ROOT / "infra" / "ansible" / "group_vars"
    wrapper = ROOT / "scripts" / "with_secrets.sh"
    if not wrapper.is_file():
        problems.append("scripts/with_secrets.sh is missing, so no deploy can carry a secret")
        return

    exported = set(re.findall(r"^\s*export\s+([A-Z0-9_]+)=", wrapper.read_text(), re.M))

    for path in sorted(group_vars.rglob("*.yml")):
        code = "\n".join(
            line for line in path.read_text().splitlines()
            if not line.lstrip().startswith("#")
        )
        for name in sorted(set(re.findall(r"lookup\(\s*'env'\s*,\s*'([A-Z0-9_]+)'\s*\)", code))):
            if name in exported:
                continue
            problems.append(
                f"{path.relative_to(ROOT)}: reads {name} from the environment, but "
                f"scripts/with_secrets.sh never exports it. The lookup resolves to "
                f'"" on every deploy, so whatever it configures is silently skipped. '
                f"Add: export {name}=\"$(read_key <key_in_the_sops_file>)\"")


def main() -> int:
    for check in (
        check_no_inbound_rules,
        check_ssh_closed_by_default,
        check_nodes_bootstrap_without_inbound,
        check_cloudinit_template_escaping,
        check_dns_records_are_declared,
        check_access_bypasses_are_declared,
        check_tfvars_hold_no_secrets,
        check_account_level_settings,
        check_spend_limit_not_in_terraform,
        check_service_required_vars_are_not_env_lookups,
        check_every_binary_install_carries_the_code_tag,
        check_example_env_is_complete_and_not_ours,
        check_every_installed_binary_has_a_source,
        check_restart_handlers_can_fire_on_a_tagged_deploy,
        check_a_job_deadline_stays_under_the_cell_ceiling,
        check_every_ansible_secret_is_exported_by_the_deploy,
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
