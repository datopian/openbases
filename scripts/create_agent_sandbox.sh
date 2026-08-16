#!/usr/bin/env bash
# Create the sandbox repository agents practise against.
#
# Agents must not learn to push against a repository that matters. Every
# repository the App is installed on today is real Datopian work — including a
# restricted client one — so the first end-to-end pull request needs somewhere
# that is genuinely disposable.
#
# Committed and idempotent because it makes a persistent GitHub change, and
# every such change has to be reproducible from the repository rather than
# remembered from a terminal session.
#
# Requires: gh, authenticated with the `repo` scope.
set -euo pipefail

ORG="${WG_SANDBOX_ORG:-datopian}"
NAME="${WG_SANDBOX_REPO:-workgraph-agent-sandbox}"
FULL="$ORG/$NAME"

if gh repo view "$FULL" >/dev/null 2>&1; then
  echo "  $FULL already exists"
else
  # Private. The contents are throwaway, but a public repository under the
  # Datopian name is a public statement, and agent practice runs are not one we
  # want to make.
  gh repo create "$FULL" \
    --private \
    --description "Disposable target for Workgraph agent runs. Nothing here is production." \
    --add-readme
  echo "  created $FULL"
fi

# A branch protection rule is deliberately NOT set. The point of the sandbox is
# to exercise the same flow production will use, and the pilot is running
# without protected branches by decision (see the GitHub plan note in the ADRs).

cat <<NOTE

  Next, one manual step that cannot be done from a token:

    Add $FULL to the Workgraph GitHub App installation.
    github.com/organizations/$ORG/settings/installations -> Workgraph -> Configure
    -> Repository access -> add $NAME -> Save

  An App cannot add repositories to its own installation, and the CLI's user
  token is not authorised against the App, so this is a human action by design.
NOTE
