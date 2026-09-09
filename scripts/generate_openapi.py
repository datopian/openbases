import json, re, os, sys, collections

pkg = 'cmd/control-api'
src = "".join(open(os.path.join(pkg,f)).read() for f in sorted(os.listdir(pkg))
              if f.endswith('.go') and not f.endswith('_test.go'))
routes = sorted(set(re.findall(r'authed\.HandleFunc\("((?:GET|POST|PATCH|PUT|DELETE) [^"]+)"', src)))

DESC = {
 "GET /v1/me": ("Who the caller is", "Identity of the already-authenticated caller. Discloses nothing they do not already hold, and is the cheapest way for a client to check a credential works."),
 "GET /v1/work": ("Work items, with what each has cost", "Scoped by row-level security to what the caller may see."),
 "GET /v1/work/queue": ("The queue, including plan jobs", "Plan jobs have no bead until they produce some, so they appear here and not in /v1/work."),
 "POST /v1/work/plan": ("Queue a planning job", "Turns a brief into beads. Costs money: an agent runs."),
 "POST /v1/work/{bead}/dispatch": ("Dispatch one bead", "Starts an agent. Costs money, and is gated by the per-bead budget (ADR-0022) and the per-token daily cap."),
 "GET /v1/projects": ("Projects the caller can see", ""),
 "GET /v1/projects/{slug}": ("One project", ""),
 "GET /v1/projects/{slug}/detail": ("A project with repositories and derived signals", "Each signal carries its evidence."),
 "GET /v1/candidates": ("The knowledge review queue", "Extracted candidates awaiting a decision, oldest first. Each carries the cited source excerpts, the meeting participants, whether the statement was said outright or inferred, and whether the source contained instruction-shaped text \u2014 everything a reviewer needs without a second request."),
 "POST /v1/candidates/{id}/review": ("Decide on one candidate", "accept, edit_and_accept, reject or defer. A rejection requires a reason and an edit requires the corrected statement, because both are the evaluation data the improvement loop reads. Answers 409 if somebody has already decided. Personal API tokens may not carry knowledge.review: a reviewer is always a named human."),
 "GET /v1/projects/{slug}/events": ("Google Workspace events belonging to a project", "Meetings and file changes the platform noticed, most recent first. Sources are linked to a project one at a time: a Meet space belongs to exactly one, while a shared drive serves many and its events are not claimed here. Metadata only \u2014 the raw Pub/Sub payload is not returned."),
 "GET /v1/ask": ("Ask the chief of staff", "Answers exactly four questions. Called with no q, it returns the supported set rather than guessing — which is the discoverability this endpoint would otherwise lack."),
 "GET /v1/inbox": ("What needs the caller", ""),
 "GET /v1/inbox/branches": ("The inbox, grouped, with a reason when a branch is empty", ""),
 "POST /v1/attention/{id}/snooze": ("Snooze an attention item", "Until a time the caller names. A snooze ending in the past is refused."),
 "POST /v1/attention/{id}/delegate": ("Hand an item to someone else", "The item MOVES rather than being copied: two people each believing the other is handling it is the failure delegation exists to remove."),
 "POST /v1/attention/{id}/resolve": ("Close an attention item", ""),
 "POST /v1/approvals/{id}/decide": ("Decide an approval", "Never reachable with an API token, at three layers: the scope cannot be stored, cannot be minted, and the route requires an action no token's scope set contains (ADR-0009, ADR-0025)."),
 "POST /v1/tokens": ("Mint a personal API token", "Requires an interactive Access session. A token cannot mint a token: a leaked credential able to produce its own successor makes revocation meaningless. The secret is returned exactly once and never stored."),
 "GET /v1/tokens": ("List the caller's tokens", "Includes revoked and expired ones, so 'why did this stop working' stays answerable. Never returns a secret."),
 "DELETE /v1/tokens/{id}": ("Revoke a token", "Effective on the next request; there is no cache in front of the lookup."),
 "GET /v1/events": ("Events since a cursor", "Cursor-paginated. The cursor is an event id, not an offset: an offset shifts under inserts, so a client paging an append-only log with OFFSET silently skips rows exactly when the log is busiest."),
 "GET /v1/events/stream": ("Follow events as they happen", "Server-sent events. Send Last-Event-ID to resume where you left off; with no cursor the stream starts from now rather than replaying history. Use this instead of polling: an agent polling /v1/work per agent forever is a cost with no ceiling."),
 "GET /v1/budget/check": ("Whether a bead may be dispatched", "Execution-node path, authorised by its own path-bound Access application."),
 "POST /v1/agent-health": ("Report agent health", "Execution-node path (ADR-0019)."),
 "POST /v1/node/work/claim": ("Claim a queued job", "Execution-node path."),
 "POST /v1/node/work/{id}/result": ("Report a job result", "Execution-node path."),
 "POST /v1/node/work/{id}/heartbeat": ("Report that a running job is still alive",
   "Execution-node path. Bytes produced and time since the last write, so a long run can be told from a stuck one."),
 "POST /v1/node/work/project": ("Project work into the graph", "Execution-node path."),
 "POST /v1/integrations/github/installation-token": ("Mint a repository-scoped git credential", "Execution-node path. Short-lived by construction (plan section 9.2)."),
}

ERR = {"type":"object","required":["error"],"properties":{
    "error":{"type":"string","description":"Human-readable. Never names which check failed."},
    "code":{"type":"string","description":"Stable machine-readable code from a closed set."}}}

CODES = ["token_scope_insufficient","role_grant_missing","route_not_declared","rate_limited",
         "spend_cap_reached","idempotency_key_conflict","invalid_idempotency_key",
         "token_self_service_forbidden","not_implemented","invalid_expiry","scope_or_lifetime_refused","invalid_until","invalid_cursor",
         # Codes added with the endpoints that emit them. The committed
         # openapi.json is the source of truth and CI does not regenerate it,
         # so these are here to stop a regeneration dropping them.
         "scope_refused","scope_unknown","invalid_project","slug_taken","not_found",
         "origin_refused","no_rig_for_project","rig_holds_no_repository",
         "bead_not_projected","refused"]

paths = collections.OrderedDict()
for r in routes:
    method, path = r.split(" ", 1)
    summary, desc = DESC.get(r, ("", ""))
    if not summary:
        print("MISSING DESCRIPTION:", r); sys.exit(1)
    params = []
    for name in re.findall(r'\{(\w+)\}', path):
        params.append({"name":name,"in":"path","required":True,"schema":{"type":"string"}})
    op = {"summary":summary,"operationId":re.sub(r'\W+','_',r).strip('_').lower(),
          "responses":{
            "200":{"description":"Success","content":{"application/json":{"schema":{"type":"object"}}}},
            "401":{"description":"Not authenticated","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Error"}}}},
            "403":{"description":"Authenticated, but this credential or role may not","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Error"}}}}}}
    if desc: op["description"] = desc
    if params: op["parameters"] = params
    if method in ("POST","PATCH","PUT","DELETE"):
        op["parameters"] = params + [{"name":"Idempotency-Key","in":"header","required":False,
            "schema":{"type":"string","maxLength":255},
            "description":"Optional. A repeat with the same key and the same body replays the recorded response; with a different body it is a 409."}]
        op["responses"]["409"] = {"description":"This idempotency key was used for a different request",
            "content":{"application/json":{"schema":{"$ref":"#/components/schemas/Error"}}}}
        op["responses"]["429"] = {"description":"Rate limited for this token; see Retry-After",
            "content":{"application/json":{"schema":{"$ref":"#/components/schemas/Error"}}}}
    if method == "POST" and "/tokens" in path:
        op["responses"]["201"] = op["responses"].pop("200")
        op["responses"]["201"]["description"] = "Created. The secret appears here and nowhere else, ever."
    paths.setdefault(path, {})[method.lower()] = op

spec = {
 "openapi":"3.1.0",
 "info":{"title":"Workgraph control API","version":"1",
   "description":"The API behind the Workgraph interface, and the contract a tool integrates against.\n\nTwo credentials reach it. An interactive Cloudflare Access session, which the interface uses; and a personal API token, which a tool uses (ADR-0025). They resolve to the SAME application user, so every row-level security policy decides identically — authentication provenance is not an input a policy has. A token can only ever be narrower than its owner, never wider.\n\nConventions, enforced by a test rather than described here in hope: every collection is enveloped under a named key, never a bare array; every timestamp is RFC 3339 in UTC; every error is {error, code} with code from a closed set; every write accepts an optional Idempotency-Key."},
 "servers":[{"url":"https://api-staging.openbases.com","description":"Staging, token-authenticated. Cloudflare Access does not challenge this hostname; the application authenticates every request itself."},
            {"url":"https://work-staging.openbases.com","description":"Staging, session-authenticated. Access challenges this hostname."}],
 "security":[{"bearerToken":[]}],
 "components":{
   "securitySchemes":{"bearerToken":{"type":"http","scheme":"bearer",
      "description":"A personal API token, prefixed wgp_. Mint one at POST /v1/tokens with an Access session; a token cannot mint a token."}},
   "schemas":{"Error":ERR,
     "ErrorCode":{"type":"string","enum":CODES,
       "description":"Stable codes. token_scope_insufficient and role_grant_missing are deliberately distinct: one means this credential may not, the other means you may not, and they have different fixes."}}},
 "paths":paths,
}
json.dump(spec, open('internal/apispec/openapi.json','w'), indent=2)
print("wrote internal/apispec/openapi.json:", len(paths), "paths,", len(routes), "operations")
