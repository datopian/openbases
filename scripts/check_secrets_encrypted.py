#!/usr/bin/env python3
"""Every credential in infra/secrets must actually be encrypted.

SOPS encrypts by key NAME, matching `encrypted_regex` in /.sops.yaml. That makes
the naming a security boundary rather than a convention, and boundaries that
depend on someone naming a field correctly need a check.

The failure this prevents is quiet and complete. Add `githubToken:` to an
encrypted file — camelCase, so it does not match the snake_case pattern — and
SOPS writes it in clear, the file still says "sops:" at the bottom, the diff
still looks like an encrypted file, and review passes. gitleaks may not catch it
either, because a token whose format it does not recognise is just a string.

So this checks three things:

  a value whose key name looks like a credential IS ciphertext
  a value in clear does not LOOK like a credential, whatever it is called
  the file is actually SOPS-managed at all

Runs in CI with no key material: it reads ciphertext markers, never decrypts.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SECRETS = ROOT / "infra" / "secrets"

# Must mirror encrypted_regex in /.sops.yaml. Deliberately duplicated rather
# than parsed out of it: if the two drift, this check failing is the signal.
CREDENTIAL_NAME = re.compile(
    r"^(.*_token|.*_secret|.*_password|.*_passphrase|.*_key|.*_credential)$"
)

# Values that are credential-SHAPED regardless of what the key is called, so a
# misnamed field is caught by its content as well as its name.
#
# arc_ was added when the first PortalJS Arc token was stored. That one was
# encrypted only because its key happened to be named portaljs_token, which
# ends in _token -- name it portaljs_arc or arc_credentials and SOPS writes it
# in clear, with nothing here recognising the value either. Checked by planting
# one: without this the script prints "every credential-named value is
# ciphertext" while a live token sits beside it in plain text.
#
# The name rule and the shape rule are meant to be two independent nets. For
# this credential there was only one.
CREDENTIAL_SHAPE = re.compile(
    r"(cfat_|gh[pousr]_|github_pat_|sk-[A-Za-z0-9]|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}"
    r"|GOCSPX-|-----BEGIN [A-Z ]*PRIVATE KEY-----|xox[baprs]-|arc_[A-Za-z0-9]{16})"
)

CIPHERTEXT = "ENC[AES256_GCM"

problems: list[str] = []


def check(path: pathlib.Path) -> None:
    text = path.read_text()
    rel = path.relative_to(ROOT)

    if "sops:" not in text:
        problems.append(f"{rel}: not SOPS-managed; it has no sops metadata block")
        return

    # A deliberately simple line scan. Parsing YAML would need a dependency CI
    # does not have, and the shape here is flat key/value by construction.
    in_block = False
    for i, line in enumerate(text.splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue

        # Indented lines are either a block scalar's body (the PEM) or the
        # inside of the sops metadata mapping. Neither carries a top-level key
        # of its own, and the block's key was checked when the block opened.
        if line.startswith((" ", "\t")):
            continue

        in_block = line.rstrip().endswith("|") or line.rstrip().endswith("|-")

        key, sep, value = line.partition(":")
        if not sep:
            continue
        key = key.strip()
        value = value.strip()

        # The sops metadata mapping is not secret material, but scanning must
        # NOT stop here. SOPS writes that block last, so an earlier version of
        # this check — which broke on it — was blind to anything appended to the
        # file afterwards. A planted token sitting after the block went
        # unreported. Skip the key, keep scanning.
        if key == "sops":
            continue

        if CREDENTIAL_NAME.match(key):
            if in_block or CIPHERTEXT not in value:
                problems.append(
                    f"{rel}:{i}: '{key}' matches the credential pattern but is not "
                    f"ciphertext. Check encrypted_regex in .sops.yaml, then re-encrypt."
                )
        elif CREDENTIAL_SHAPE.search(value):
            problems.append(
                f"{rel}:{i}: '{key}' is stored in clear and its value looks like a "
                f"credential. Rename it to end in _token, _secret, _password, "
                f"_passphrase, _key or _credential so SOPS encrypts it."
            )


def check_template(path: pathlib.Path) -> None:
    """A template names what a deployment needs and holds none of it.

    The whole point of moving the real files out of a public repository is
    undone by one filled-in template, and a filled-in template looks exactly
    like a helpful example until you read the values.
    """
    rel = path.relative_to(ROOT)
    for i, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, sep, rest = line.partition(":")
        if not sep:
            continue
        # Strip a trailing comment, then whatever quoting the value carries.
        value = rest.split("#", 1)[0].strip().strip("\"'")
        if value:
            problems.append(
                f"{rel}:{i}: '{key.strip()}' has a value. A template in a public "
                f"repository must name the setting and hold nothing: put the real "
                f"value in the encrypted file outside this repository.")


def main() -> int:
    if not SECRETS.is_dir():
        print("no infra/secrets directory; nothing to check")
        return 0

    # The templates are the thing this repository is REQUIRED to carry.
    #
    # The real .enc.yaml files moved out when the repository went public, so
    # the old "no encrypted secret files found -> return 0" made this check a
    # silent pass on an empty directory: delete everything and CI still says
    # OK. What must exist here is a template per environment, and what must be
    # true of a template is that it holds no values.
    templates = sorted(SECRETS.glob("*.example.yaml"))
    if not templates:
        print("FAILED:")
        print(f"  - {SECRETS.relative_to(ROOT)}: no *.example.yaml template. The public "
              f"repository must document what a deployment needs, even though it "
              f"no longer carries the values.")
        return 1
    for path in templates:
        check_template(path)

    # Any encrypted file that IS here is still checked, so a private fork that
    # keeps one in place is held to the same rule.
    files = sorted(SECRETS.glob("*.enc.yaml"))
    for path in files:
        check(path)

    print(f"encrypted secrets check ({len(templates)} template(s), {len(files)} encrypted file(s))")
    if problems:
        print("FAILED:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("every credential-named value is ciphertext")
    return 0


if __name__ == "__main__":
    sys.exit(main())
