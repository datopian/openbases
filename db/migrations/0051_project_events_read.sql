-- wg:backfill — links the two Meet sources to their projects.
--
-- A read path for Workspace events, scoped to the project they belong to.
--
-- Until now event_sources, event_subscriptions and event_receipts had RLS
-- FORCED with ZERO policies, so nothing reachable by a person could read them:
-- the only way in was the SECURITY DEFINER system_* functions the reconciler
-- uses. That was right while nothing consumed events, and wrong as soon as
-- somebody asked "did the platform notice my kick-off?" -- because the honest
-- answer was "yes, and nobody can see that it did".
BEGIN;

-- Which project a source belongs to. NULL means none, which is not a gap:
--
--   A Meet space belongs to exactly one project. That is the whole point of
--   asking PMs for one space per project.
--
--   A shared drive does NOT. All, BizDev and Delivery each carry material for
--   many projects, and the event names a FILE, not the folder it lives in --
--   so attributing a Drive change to a project needs the file's parents from
--   the Drive API, which is a separate piece of work. Until then these stay
--   NULL and their events are visible to any authenticated user, which is what
--   'internal' already meant for them.
ALTER TABLE event_sources
    ADD COLUMN IF NOT EXISTS project_id uuid REFERENCES projects(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS event_sources_project ON event_sources (project_id);

COMMENT ON COLUMN event_sources.project_id IS
  'The project a source belongs to, or NULL for a shared drive that serves many (WP-H3).';

-- The CDT kick-off space belongs to CDT.
UPDATE event_sources s
   SET project_id = (SELECT id FROM projects WHERE slug = 'cdt'), updated_at = now()
 WHERE s.kind = 'meet' AND s.external_id = '44KbrlezvqkB';

-- Innovation Team Sync is the DataHub.io product meeting, and DataHub.io lives
-- inside datopian-products.
UPDATE event_sources s
   SET project_id = (SELECT id FROM projects WHERE slug = 'datopian-products'), updated_at = now()
 WHERE s.kind = 'meet' AND s.external_id = 'FS4Sj-9MIY0B';

-- Read policies, reusing can_read_source rather than writing a second rule.
--
-- can_read_source(project_id, visibility) already means exactly what is wanted
-- here: a project-scoped row defers to can_read_project, and a row with no
-- project is readable when it is 'internal'. Reusing it keeps ONE authorisation
-- model for the whole schema. A second predicate that happened to agree today
-- is a second predicate that drifts.
--
-- SELECT only. No insert, update or delete policy: everything that writes these
-- tables goes through the SECURITY DEFINER system_* functions, and a FOR ALL
-- policy would silently re-open reads -- the mistake 0008 records
-- rls_isolation.sql having caught once already.
CREATE POLICY event_sources_read ON event_sources FOR SELECT
    USING (can_read_source(project_id, visibility));

-- A receipt is as visible as the source that produced it.
--
-- A receipt with NO source is deliberately invisible. Those are deliveries from
-- something we do not allow-list -- the "a non-allow-listed source is ignored"
-- case -- and their target names a resource nobody has decided anything about.
-- They stay readable through the system path, where an operator investigating
-- can see them.
CREATE POLICY event_receipts_read ON event_receipts FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM event_sources s
         WHERE s.id = event_receipts.source_id
           AND can_read_source(s.project_id, s.visibility)
    ));

COMMIT;
