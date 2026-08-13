# Pilot projects, people and roles

The source of truth for this is the control-plane registry (`projects`,
`project_memberships`, `role_grants`), created by WP-C3. This file records the decisions so they
exist in Git before the registry does, and so WP-C3 has something to seed from rather than a
conversation to reconstruct.

**Bead:** `wg-8yv.37` (projects) and `wg-8yv.39` (people and roles).

## People

Four pilot identities, all authorised on the Cloudflare Access policy for both environments.

| Person | Email | Organisation role |
|---|---|---|
| Anuar Ustayev | `anuar.ustayev@datopian.com` | `organisation_admin` |
| Rufus Pollock | `rufus.pollock@datopian.com` | `executive` |
| Osahon Okungbowa | `osahon.okungbowa@datopian.com` | — (project-scoped only) |
| Daniela Popova | `daniela.popova@datopian.com` | `function_lead` (marketing) |

Roles are **scoped grants**, not global titles: `role_grants` carries an optional `project_id`, so
the same person is a lead on one project and an observer on another. The organisation role above is
the grant with a null `project_id`.

> These assignments were proposed rather than specified, and accepted as a starting point. They are
> cheap to change until WP-C3 writes them into the registry.

## Projects

Plan §3.1 requires exactly three: one open-source, one internal/product, one restricted client in
its own execution cell.

### `portaljs-oss` — open source

Portfolio `oss`, visibility `internal`, policy bundle `open-source`. May share an execution cell
with other non-sensitive work.

| Repository |
|---|
| `datopian/portaljs` |
| `datopian/wayintoai` |
| `datopian/giftless` |
| `datopian/autoclaw.sh` |
| `ckan/ckan` |
| `flowershow/flowershow` |

**Note:** `ckan/ckan` and `flowershow/flowershow` are **outside the `datopian` organisation**. The
GitHub App must be installed on those organisations separately, and that needs an owner of each to
approve it. `ckan/ckan` in particular is a community project — installing an app that can write to
it is a governance question for its maintainers, not only for Datopian. Treat it as the last
repository to onboard, or drop it from the pilot.

| | |
|---|---|
| Primary operator | **TBD** |
| Backup operator | **TBD** |

### `datopian-products` — internal / product

Portfolio `product`, visibility `internal`, policy bundle `default`.

| Repository |
|---|
| `datopian/postal-codes` |
| `datopian/sre-agent` |
| `datopian/cloud.portaljs.com` |
| `datopian/datahub-next` |

| | |
|---|---|
| Primary operator | Anuar Ustayev |
| Backup operator | Daniela Popova |

### `nged` — restricted client

Portfolio `client`, visibility **`restricted`**, policy bundle `client-restricted`.

| Repository |
|---|
| `datopian/nged` |

| | |
|---|---|
| Primary operator | Osahon Okungbowa |
| Backup operator | Rufus Pollock |

Constraints that follow from `restricted` (ADR-0002, ADR-0013, `policies/client-restricted.yaml`):

- its own execution cell, with a dedicated Linux user and credential profile. The database refuses a
  restricted project with no `execution_cell_id`;
- external publication blocked by default; a marketing packet requires the `client-approved` flag;
- its content never enters company-level summarisation or a cross-project context pack;
- derived records inherit `restricted` transitively, including embeddings and search results.

**Outstanding:** any contractual constraint on data handling, retention, naming, or jurisdiction for
NGED. Retention class defaults to `standard`; a client contract usually means `client-contract`. This
needs answering before the project holds real client material.

## Open items

1. **Primary and backup operator for `portaljs-oss`.** The registry rejects a project where these
   are the same person (`backup_owner_differs`), so this needs two distinct names.
2. **NGED contractual constraints** — retention class, data handling, whether the client name may be
   used at all.
3. **GitHub App installation** on `ckan` and `flowershow` organisations, each needing an owner's
   approval.
