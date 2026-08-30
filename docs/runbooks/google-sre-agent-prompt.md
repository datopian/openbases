# Prompt for an SRE agent with gcloud access

Copy everything between the rules into the agent. It does the Google Cloud half
of [`google-workspace-consent.md`](google-workspace-consent.md); the Workspace
Admin console half cannot be done with `gcloud` and stays with a human.

---

You are setting up a minimal Google Cloud project that will act **solely** as
the Pub/Sub delivery fabric for Google Workspace Events. Nothing else will ever
be deployed into it: no application, no data at rest, no other workloads. The
consuming system runs on our own infrastructure and only receives push
deliveries from this project.

Work in this order and stop at the first step that fails rather than working
around it.

## 1. Project

Create a project named `datopian-workgraph-events`, or tell me if one with that
name already exists rather than creating a second.

```bash
gcloud projects create datopian-workgraph-events \
  --name="Datopian Workgraph Events"
```

Link it to the organisation's billing account. Pub/Sub has a free tier that this
will almost certainly stay inside, but API enablement requires billing to be
linked.

Report the **project ID**, which may differ from the name if that name was taken.

## 2. Enable exactly four APIs

```bash
gcloud services enable \
  pubsub.googleapis.com \
  workspaceevents.googleapis.com \
  meet.googleapis.com \
  drive.googleapis.com \
  --project <PROJECT_ID>
```

Four, and only four. Do not enable anything else, however useful it looks — the
value of a project with one job is that its IAM and its attack surface can be
read at a glance.

## 3. Service account

```bash
gcloud iam service-accounts create workgraph-events \
  --display-name="Workgraph Workspace Events" \
  --project <PROJECT_ID>
```

Grant it exactly two roles, on this project only:

```bash
gcloud projects add-iam-policy-binding <PROJECT_ID> \
  --member="serviceAccount:workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com" \
  --role="roles/pubsub.admin"

gcloud projects add-iam-policy-binding <PROJECT_ID> \
  --member="serviceAccount:workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com" \
  --role="roles/serviceusage.serviceUsageAdmin"
```

`pubsub.admin` because Terraform creates and manages the topic and subscription.
`serviceUsageAdmin` because it re-asserts the four APIs above.

**Do not grant `roles/owner` or `roles/editor`.** If something later fails with a
permission error, tell me what it was and let the grant be a decision rather than
a fix — a broad role added to make an error go away is how a service account
ends up able to do things nobody chose.

Do **not** grant this service account any role at the organisation or folder
level. Project scope only.

## 4. Key

```bash
gcloud iam service-accounts keys create /dev/stdout \
  --iam-account=workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com
```

Written to stdout, not to a file: this is a long-lived credential for an account
that will hold domain-wide read access to Meet and Drive, and it must not land
on local disk — least of all inside a git working tree.

Hand the JSON to me directly. It goes into our own encrypted secrets file, which
is the repository's vault: SOPS-encrypted, in Git, reviewed, and decrypted into
a directory that dies with the process. You do not need 1Password or any other
password manager for this.

If the organisation forbids service account keys
(`iam.disableServiceAccountKeyCreation` **enforced**, not merely defined), stop
and tell me — Workload Identity Federation is a better answer and changes what I
ask for, but I would rather know than have the constraint worked around.

## 5. The client ID I need

```bash
gcloud iam service-accounts describe \
  workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com \
  --project <PROJECT_ID> \
  --format='value(oauth2ClientId)'
```

That numeric OAuth 2.0 client ID is what a Workspace administrator types into
the domain-wide delegation screen. It is not the email and not the unique ID.

## 6. What you cannot do, and should not try

**Domain-wide delegation is not a `gcloud` operation.** It is set in the Google
Workspace Admin console under Security → Access and data control → API controls
→ Domain-wide delegation, by someone with Workspace super-admin rights. Do not
attempt it through the Directory API or by any other route; it is deliberately a
human approval, because it grants read access to every meeting recording and
every Drive file in the tenant.

For the record, the scopes that will be granted there are exactly two:

```
https://www.googleapis.com/auth/meetings.space.readonly
https://www.googleapis.com/auth/drive.readonly
```

If you are asked to add more, or a different scope appears to be needed, say so
rather than adding it.

## 7. Verify, then report

```bash
gcloud services list --enabled --project <PROJECT_ID> \
  | grep -E 'pubsub|workspaceevents|meet|drive'

gcloud projects get-iam-policy <PROJECT_ID> \
  --flatten="bindings[].members" \
  --filter="bindings.members:workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com" \
  --format="table(bindings.role)"
```

The second command should print exactly two roles. If it prints more, say which
and why before I use the project.

Report back:

- the **project ID**
- the service account **email**
- the **oauth2ClientId** from step 5
- the **key JSON** itself, handed over directly
- the output of both verification commands
- anything you had to do differently, and why

Do not create the Pub/Sub topic or subscription. Those are managed by OpenTofu
from our repository, so creating them by hand would produce resources that our
state does not know about and that the next apply would fight.

---

## After the agent reports back

Two things remain, both human:

1. **Domain-wide delegation** with the two scopes, using the `oauth2ClientId` the
   agent returns (step 4 of the consent runbook).
2. **Confirm whether Drive Workspace Events is production-approved for the
   tenant**, or whether the `changes.watch` / `changes.list` fallback is needed.
   This changes what WP-H1 builds.

Then `google_project_id` goes into `infra/tofu/envs/staging/terraform.tfvars`
and one apply creates the topic and subscription.
