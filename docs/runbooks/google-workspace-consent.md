# Enabling the Google Workspace connector (wg-8yv.34)

WP-H1 ingests Meet transcripts and Drive changes. Everything it needs from
Google is listed here. It is one blocker with five parts, and it gates five
Phase H P0s — nothing in Phase H starts without it.

**What Google is used for, and what it is not.** A minimal Google Cloud project
acts solely as the Pub/Sub delivery fabric for Workspace Events. No application
runs there, no data is stored there, and nothing else is deployed to it. The
connector runs on our own control node.

Everything below is a human action with administrative authority. None of it can
be scripted from here, which is why this is a runbook rather than a script.

## 1. A Google Cloud project

Either create one — `datopian-workgraph-events` is a reasonable name — or
nominate an existing one to reuse. Note the **project ID**, which is not the
display name.

Nothing else should be deployed into it. That is a deliberate blast-radius
decision, not tidiness: a project whose only resource is a Pub/Sub topic is one
whose IAM you can reason about.

## 2. Enable four APIs on that project

```
pubsub.googleapis.com
workspaceevents.googleapis.com
meet.googleapis.com
drive.googleapis.com
```

`gcloud services enable pubsub.googleapis.com workspaceevents.googleapis.com meet.googleapis.com drive.googleapis.com --project <PROJECT_ID>`

OpenTofu also enables these, so this step is belt and braces — but the apply in
step 6 fails without the Pub/Sub API already on, which makes it worth doing by
hand first.

## 3. A service account, and a key for OpenTofu

Create a service account in that project — `workgraph-events` — and grant it, on
that project only:

| Role | Why |
|---|---|
| `roles/pubsub.admin` | creates and manages the topic and subscription |
| `roles/serviceusage.serviceUsageAdmin` | enables the four APIs above |

Then create a JSON key for it. That key is what lets OpenTofu manage the
project. Send it through 1Password, not email or Slack.

## 4. Domain-wide delegation, and the scopes

The connector reads Meet and Drive on behalf of the Workspace, so the same
service account needs domain-wide delegation. In the **Google Workspace Admin
console** → Security → Access and data control → API controls → Domain-wide
delegation, add the service account's **client ID** with exactly these two
scopes:

```
https://www.googleapis.com/auth/meetings.space.readonly
https://www.googleapis.com/auth/drive.readonly
```

**Two, and only two.** These come from Google's own discovery documents, not
from memory: every `conferenceRecords` read — transcripts, entries,
participants, recordings — is authorised by `meetings.space.readonly`, and
`changes.watch`, `changes.list` and `files.get` are all authorised by
`drive.readonly`. Creating a Workspace Events subscription is authorised by
whichever scope covers the resource being watched, so these two cover that too.

Do not grant `meetings.space.created`, and do not grant plain `drive`. The first
is for spaces the app itself created, which is not our case; the second is
write access to every file in the domain, to do a job that only reads.

**This is the step that needs your explicit approval as an administrator.** It
grants an automated system read access to every meeting recording and every
Drive file in the tenant. That is the whole point of it being a human decision
recorded in a bead rather than a line of Terraform.

## 5. One question I cannot answer from here

**Is Drive Workspace Events enabled and production-approved for the Datopian
tenant?**

Drive events through the Workspace Events API have been gated per-tenant. If
they are not available, the connector must use the stable `changes.watch` and
`changes.list` fallback instead — which works, but is a different subscription
lifecycle and a different piece of code.

Check in the Admin console, or attempt a Drive subscription once step 4 is done.
The answer changes what WP-H1 builds, so it is worth establishing before the
code is written rather than after.

## 6. Then, from the repository

Put the project ID in `infra/tofu/envs/staging/terraform.tfvars`:

```hcl
google_project_id = "<PROJECT_ID>"
```

Point `GOOGLE_APPLICATION_CREDENTIALS` at the JSON key from step 3, and apply:

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json
scripts/with_secrets.sh staging scripts/tofu.sh staging apply
```

Until `google_project_id` is set the module is `count = 0` and produces no
Google resources at all, so this changes nothing for anyone who has not done the
steps above. That is deliberate: a plan should be honest for people with no
Google credentials, which is everyone until this is turned on.

## What to send back

- the **project ID**
- the service account **JSON key** (1Password)
- the service account **client ID**, so the delegation can be verified
- **yes or no** on Drive Workspace Events for the tenant (step 5)

With those, WP-H1 (`wg-8yv.20`) is unblocked and the other four Phase H P0s
behind it can start.

## A decision that is not yours to make alone

Pub/Sub pushes to `https://<hostname>/v1/google/events`, and that endpoint is
behind Cloudflare Access. Google cannot complete an Access challenge, so it
needs either a service-token bypass on that one path or a verified push
subscription with the OIDC token checked in the handler.

That is the same shape as the GitHub webhook bypass, and it should be decided
the same way — as a reviewed change with its reasoning written down, not as a
line added during a deploy. It is WP-H1's first design decision and does not
block anything here.
