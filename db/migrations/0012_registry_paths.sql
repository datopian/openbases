-- 0012_registry_paths.sql — record where each Beads database lives, and make
-- work-reference identity NULL-safe.
-- Work package: WP-D2. Plan sections 2.3, 7.3.

BEGIN;

-- Where the database's working directory is on its execution node.
--
-- Held in the registry rather than discovered on disk: a graph the control
-- plane has no record of is one it must not run bd against, because it cannot
-- know the trust domain it belongs to.
ALTER TABLE beads_databases ADD COLUMN IF NOT EXISTS path text;

-- work_refs identity, made NULL-safe.
--
-- The original UNIQUE (organisation_id, execution_cell_id, beads_database_id,
-- bead_id) does not behave as intended when execution_cell_id is NULL, which is
-- the normal case for a company or personal graph that belongs to no cell.
-- Postgres treats NULLs as distinct, so the constraint never matches, ON
-- CONFLICT never fires, and the same bead is inserted repeatedly — each new row
-- silently orphaning the links that pointed at the previous one.
--
-- A unique index over COALESCE closes that. The all-zero UUID is a sentinel for
-- "no cell"; it is never a real execution_cell_id because those are generated.
ALTER TABLE work_refs DROP CONSTRAINT IF EXISTS work_refs_organisation_id_execution_cell_id_beads_database_key;

CREATE UNIQUE INDEX IF NOT EXISTS work_refs_identity_uq
    ON work_refs (
        organisation_id,
        COALESCE(execution_cell_id, '00000000-0000-0000-0000-000000000000'::uuid),
        beads_database_id,
        bead_id
    );

COMMIT;
