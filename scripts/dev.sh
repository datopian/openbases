#!/usr/bin/env bash
# Run the local development stack.
#
# The local stack is web, API, PostgreSQL, fixture Beads, a sandbox Gas Town
# cell, and mocked GitHub and Google events. Ordinary feature work must never
# require SSH to staging or production (plan section 16.4).
set -euo pipefail

cd "$(dirname "$0")/.."

echo "The local stack is assembled by WP-C1 (database), WP-D1 (GitHub fixtures)"
echo "and WP-E1 (sandbox cell). Until those land, run the API directly:"
echo
echo "  WG_ENV=local WG_DATABASE_URL=postgres://localhost/workgraph go run ./cmd/control-api"
echo
exit 1
