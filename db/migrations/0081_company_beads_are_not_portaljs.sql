-- wg:backfill — company beads stop being filed under PortalJS (wg-43n follow-up).
--
-- 0080 moved the sandbox beads off portaljs-oss and left 101 behind, because
-- the filter was `bead_id LIKE 'sa-%'` and that was derived from a sample of
-- eight rows which — ordered by bead_id — happened to all be `sa-`. Grouping
-- rather than sampling would have shown two prefixes:
--
--     prefix  count
--     wg      192
--     sa       29
--
-- So 101 `wg-` beads stayed attached to portaljs-oss, which 0080 had just
-- closed and emptied of repositories. A closed project holding a hundred beads
-- from the company graph is worse than the state before.
--
-- They are not PortalJS work and they are not platform work either. `wg` is the
-- company-hq prefix, and company-hq carries company-wide decisions — the whole
-- company's work, not one project's. There is no project they belong to, and
-- inventing one would repeat the mistake that put them on portaljs-oss: they
-- were attributed by the old cell rule to whatever single project the oss cell
-- happened to map to.
--
-- project_id IS NULL is the CORRECT state for a company-scoped bead, and the
-- policy says so explicitly — work_refs_read is
-- `project_id IS NULL OR can_read_project(project_id)`, with the comment "a
-- company-scoped row stays visible without a user". So they join the 75 other
-- company beads already in that state.
--
-- The 16 Jackson beads are untouched: they carry an explicit project: label, so
-- their attribution is a statement rather than an inference.

BEGIN;

DO $$
DECLARE org uuid; v_oss uuid; n integer;
BEGIN
    SELECT id INTO org FROM organisations WHERE slug = 'datopian';
    SELECT id INTO v_oss FROM projects
     WHERE organisation_id = org AND slug = 'portaljs-oss';

    IF v_oss IS NULL THEN
        -- Already gone, or never existed on this database. Nothing to correct.
        RAISE NOTICE 'no portaljs-oss project; nothing to detach';
        RETURN;
    END IF;

    -- Company-graph beads only. A `sa-` bead on this project would be one 0080
    -- failed to move and belongs to the platform, not to nobody, so it is left
    -- for a person to look at rather than swept up here.
    SELECT count(*) INTO n
      FROM work_refs
     WHERE project_id = v_oss AND bead_id LIKE 'sa-%';
    IF n > 0 THEN
        RAISE EXCEPTION '% sandbox bead(s) are still on portaljs-oss; 0080 did not finish', n;
    END IF;

    UPDATE work_refs SET project_id = NULL WHERE project_id = v_oss;

    SELECT count(*) INTO n FROM work_refs WHERE project_id = v_oss;
    IF n <> 0 THEN
        RAISE EXCEPTION '% bead(s) are still attached to portaljs-oss', n;
    END IF;

    RAISE NOTICE 'portaljs-oss holds no work references';
END $$;

COMMIT;
