-- wg:backfill — creates the CDT pilot project.
--
-- CDT, the third pilot project (wg-8yv.37). Client engagement, classified
-- restricted with its own execution cell, decided by Anuar 2026-09-01.
--
-- Created here as CONFIDENTIAL and tightened to restricted by the execution
-- registry once the cell exists. That is not a preference, it is the only order
-- the schema allows:
--
--   CONSTRAINT restricted_requires_cell
--       CHECK (visibility <> 'restricted' OR execution_cell_id IS NOT NULL)
--
-- A migration cannot know that a host has a cell called client-cdt -- cells are
-- provisioned by Ansible against a specific node -- so the assignment and the
-- tightening live in infra/ansible/group_vars/all/registry.yml, exactly as nged
-- did before it. 0009 wrote that sequence down and this is the second use of it.
BEGIN;

INSERT INTO projects (
    organisation_id, portfolio_id, slug, name, objective, visibility,
    primary_owner_id, backup_owner_id, status
)
SELECT o.id,
       (SELECT id FROM portfolios WHERE slug = 'client'),
       'cdt',
       'CDT',
       'California Department of Technology: CKAN migration and open data platform.',
       -- Tightened to restricted with the cell. See the header.
       'confidential',
       (SELECT id FROM users WHERE lower(primary_email) = 'monika.popova@datopian.com'),
       (SELECT id FROM users WHERE lower(primary_email) = 'osahon.okungbowa@datopian.com'),
       'active'
  FROM organisations o
 WHERE o.slug = 'datopian'
   AND NOT EXISTS (SELECT 1 FROM projects WHERE slug = 'cdt');

INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.primary_owner_id, 'project_lead' FROM projects p WHERE p.slug = 'cdt'
UNION ALL
SELECT p.id, p.backup_owner_id, 'backup_operator' FROM projects p WHERE p.slug = 'cdt'
ON CONFLICT DO NOTHING;

-- The kick-off Meet source, raised to match the engagement.
--
-- Registered as confidential in 0048 before the classification was decided.
-- Raising rather than lowering is the safe direction: every artefact derived
-- from this source inherits the level, so a source that is stricter than its
-- project for a few minutes costs nothing, and the reverse is unrecoverable.
UPDATE event_sources
   SET visibility = 'restricted',
       rationale = 'Client engagement, classified restricted with its own execution '
                   'cell (wg-8yv.37, decided 2026-09-01). Commercial terms and client '
                   'material; derived artefacts must not inherit company-wide '
                   'visibility. Space code rtd-siqf-aup.',
       updated_at = now()
 WHERE kind = 'meet' AND external_id = '44KbrlezvqkB';

COMMIT;
