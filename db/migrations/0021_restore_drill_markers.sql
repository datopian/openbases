-- Evidence rows for the restore drill (WP-I2).
--
-- The drill proves a backup is restorable by writing a marker into the live
-- database, backing up, restoring into a scratch instance, and requiring the
-- marker to come back out. Without a marker the check degenerates into "does it
-- start and have tables", which a backup of an EMPTY database passes.
--
-- The table is a migration rather than something the drill creates for itself.
-- A test that runs CREATE TABLE against the live database is a schema change
-- outside the migration history: nothing reviews it, nothing records it, and the
-- next person to diff the deployed schema against db/migrations finds a table
-- with no explanation. Two lines of DDL here is the cheaper answer.
--
-- The rows are also useful on their own. Each one records that a drill ran and
-- when, so "when was the last restore actually tested" is a query rather than a
-- search through chat history.

BEGIN;

CREATE TABLE IF NOT EXISTS restore_drill_markers (
    marker     text PRIMARY KEY,
    written_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE restore_drill_markers IS
    'Markers written by test/acceptance/restore_drill.sh. A row proves a drill '
    'wrote to the live database at that time; the drill proves the row survived '
    'a backup and restore.';

-- Row-level security with no policy, the same treatment as agent_health_events
-- and github_deliveries. These rows describe infrastructure rather than project
-- work, there is no per-project scoping that would make them meaningful to a
-- member, and the drill runs as the owner. Enabling it without a policy means
-- every ordinary session sees an empty table rather than relying on nobody
-- thinking to look.
ALTER TABLE restore_drill_markers ENABLE ROW LEVEL SECURITY;
ALTER TABLE restore_drill_markers FORCE ROW LEVEL SECURITY;

COMMIT;
