-- wg:backfill — one standing project for prospect proof-of-concept work (wg-5h8).
--
-- Decided by Anuar 2026-09-03. Before this, every PoC needed a project of its
-- own, and a project needs a migration and a deploy (wg-ml3) -- so the one step
-- of the loop that could not be done from an API was the first one. A standing
-- project makes the per-prospect unit a LABEL instead, and labels need nothing.
--
-- Internal, on the oss cell. Not a new cell: cells are drawn by trust domain
-- rather than by project (plan section 7.4), and portaljs-oss and roseville-poc
-- already share oss. Nobody ever needed a workstation per PoC.
--
-- THE LINE THIS DEPENDS ON, and it belongs in the migration rather than only in
-- a bead: `poc` is for PRE-NDA, PUBLIC-DATA prospect work. That is what makes
-- one shared project safe -- every bead in it is readable by every member, and a
-- portal built from a public catalogue holds nothing anybody needs protecting
-- from. The moment a prospect shares anything under NDA, that engagement gets
-- its own project and its own cell, which is the existing restricted path and
-- is how CDT and NGED already work. A restricted project cannot even exist
-- without a cell (restricted_requires_cell), so the schema enforces the second
-- half of that rule on its own.
--
-- roseville-poc is NOT folded into this. It predates the decision, it is named
-- in the 3 September demo script, and moving a project's work to change how it
-- is grouped is not worth doing to a thing that already works.
--
-- What this migration deliberately does NOT do: remap
-- datopian/workgraph-agent-sandbox from portaljs-oss to poc.
-- project_repositories has UNIQUE (provider, owner, name), so the sandbox can
-- belong to exactly one project, and portaljs-oss holding it is what makes
-- B4's demo fallback work today. The remap is one row and is wg-jjy, to be
-- applied after the session.
BEGIN;

INSERT INTO projects (organisation_id, slug, name, objective, visibility,
                      primary_owner_id, backup_owner_id, execution_cell_id)
SELECT o.id,
       'poc',
       'Prospect proofs of concept',
       'The standing home for pre-NDA proof-of-concept work built from a prospect''s public data. One project, one sandbox; the prospect is a label.',
       'internal',
       po.id, bo.id, c.id
  FROM organisations o
  JOIN users po ON po.primary_email = 'anuar.ustayev@datopian.com'
  JOIN users bo ON bo.primary_email = 'osahon.okungbowa@datopian.com'
  LEFT JOIN execution_cells c ON c.slug = 'oss'
 WHERE o.slug = 'datopian'
ON CONFLICT (organisation_id, slug) DO NOTHING;

-- The same eight people who reach roseville-poc and portaljs-oss (0074).
--
-- Owner and backup are among them by necessity rather than by choice:
-- test/integration/pilot_registry.sql asserts that a project's primary owner is
-- a member of it, because an owner who is not is an owner who cannot see their
-- own project.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, u.id, 'contributor'
  FROM projects p
  JOIN users u ON u.primary_email IN (
        'anuar.ustayev@datopian.com',
        'daniela.popova@datopian.com',
        'osahon.okungbowa@datopian.com',
        'rufus.pollock@datopian.com',
        'joao.demenech@datopian.com',
        'monika.popova@datopian.com',
        'aleksandra.rubaj@datopian.com',
        'luccas.mateus@datopian.com')
 WHERE p.slug = 'poc'
ON CONFLICT DO NOTHING;

COMMIT;
