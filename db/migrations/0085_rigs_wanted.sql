-- Which rigs a cell should have (rig-per-repository).
--
-- 0084 recorded which repository each rig HOLDS, reported by the node, and made
-- dispatch refuse when no rig held the work. That turned a silent failure into
-- a loud one and left the obvious question: then provision the rigs.
--
-- This is the other direction. Given a cell, the repositories its projects have
-- and therefore the rigs it needs. The playbook reads it and runs `gt rig add`
-- for whatever is missing.
--
-- Provisioning rather than polling, deliberately. Creating a rig clones a
-- repository and seeds agent infrastructure -- refinery, mayor and witness
-- directories, patrol molecules, a rig-level bead database -- which is minutes
-- of work and gigabytes of disk. That belongs in a deploy somebody ran, not in
-- a fifteen-second dispatcher pass that would do it the moment a repository was
-- attached from a phone.
--
-- The name is derived here rather than in the playbook so that one answer
-- exists: the node reports rigs by directory name (0084), the registry decides
-- what those names should be, and a rig named differently in two places is a
-- rig dispatch cannot route to.

BEGIN;

-- Dropped rather than replaced: this adds a column to the returned table, and
-- CREATE OR REPLACE cannot change a function's return type.
DROP FUNCTION IF EXISTS system_rigs_wanted(text);

CREATE FUNCTION system_rigs_wanted(p_cell text)
RETURNS TABLE (
    rig       text,
    provider  text,
    owner     text,
    name      text,
    clone_url text,
    prefix    text,
    project   text,
    held      boolean
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH cell AS (
        SELECT id FROM execution_cells WHERE slug = p_cell
    ),
    repos AS (
        SELECT DISTINCT
               r.provider, r.owner, r.name,
               p.slug AS project,
               -- The repository name, lower-cased, with anything that is not a
               -- letter, digit or underscore folded to an underscore.
               --
               -- A rig name reaches a directory, a systemd unit and a git
               -- branch prefix, so the safe set is narrow. autoclaw.sh becomes
               -- autoclaw_sh and cloud.portaljs.com becomes cloud_portaljs_com
               -- rather than being silently truncated at the dot.
               regexp_replace(lower(r.name), '[^a-z0-9_]+', '_', 'g') AS base
          FROM project_repositories r
          JOIN projects p ON p.id = r.project_id
          JOIN cell c ON c.id = p.execution_cell_id
         -- Anything but closed, not just active. A paused project can still be
         -- dispatched to -- system_rigs_for_bead does not look at status -- so
         -- wanting rigs only for active projects would leave a paused project
         -- routable and unprovisioned, which is the refusal this whole change
         -- exists to remove. Closed is the one state whose work is over, and
         -- cloning a repository for it is disk spent on nothing.
         WHERE p.status <> 'closed'
    ),
    named AS (
        SELECT repos.*,
               -- Two projects on one cell may hold repositories with the same
               -- name under different owners. The bare name would collide into
               -- one directory and one of the two would silently be the wrong
               -- checkout, so a colliding name is qualified by its owner.
               CASE WHEN count(*) OVER (PARTITION BY base) > 1
                    THEN regexp_replace(lower(owner), '[^a-z0-9_]+', '_', 'g') || '_' || base
                    ELSE base
               END AS rig_name
          FROM repos
    )
    SELECT
           -- A rig that ALREADY holds this repository keeps its own name.
           --
           -- Without this the two rigs the oss cell was built with by hand
           -- would be ordered a second time under derived names: `autoclaw`
           -- holds datopian/autoclaw.sh, which derives `autoclaw_sh`, and
           -- `sandbox` holds workgraph-agent-sandbox. Cloning the same
           -- repository into a second rig is not a harmless duplicate -- two
           -- rigs holding one repository means two bead graphs and two branch
           -- namespaces for the same code, and dispatch would route to
           -- whichever the query happened to return first.
           --
           -- The repository, not the name, is the identity. That is exactly
           -- what 0084 made the node report, so this join is the one place
           -- where the hand-built past and the derived future are reconciled.
           coalesce(e.rig, n.rig_name),
           n.provider,
           n.owner,
           n.name,
           'https://github.com/' || n.owner || '/' || n.name || '.git',
           -- A short bead prefix, so ids from different rigs are
           -- distinguishable. Beads graphs cannot reference each other, so a
           -- bare id with no prefix is ambiguous the moment there is more than
           -- one graph (group_vars/control.yml says the same about the company
           -- graphs).
           --
           -- Three characters of the name plus one from a hash of the
           -- repository, and the fourth character is the important one. Three
           -- alone collided immediately on the oss cell: datahub-next and
           -- data-portal-examples both give `dat`, which would put two rigs'
           -- beads in one id space.
           --
           -- The obvious fix -- number the collisions -- is worse than it
           -- looks. Prefixes would then depend on the SET of repositories, so
           -- attaching `database-x` later would renumber datahub-next from
           -- dat2 to dat3, and bead ids already issued under dat2 would belong
           -- to a different rig. A bead id is permanent; a prefix that shifts
           -- underneath it is a corruption.
           --
           -- Hashing the repository's full name instead ties the prefix to the
           -- repository's identity and nothing else, so it is stable for as
           -- long as the repository is called what it is called. Readability
           -- survives in the first three characters, which is what somebody
           -- scanning a bead id actually uses.
           --
           -- Four characters also cannot collide with the prefixes the cell
           -- already has, which gt derived from the name and are two (`au`,
           -- `sa`); and the prefix is only read when a rig is created, so a
           -- rig that already exists keeps whatever prefix it was made with.
           substring(n.rig_name from 1 for 3)
             || substr(md5(n.owner || '/' || n.name), 1, 1),
           n.project,
           e.rig IS NOT NULL
      FROM named n
      LEFT JOIN execution_rigs e
             ON e.execution_cell_id = (SELECT id FROM cell)
            AND e.provider = n.provider
            AND lower(e.owner) = lower(n.owner)
            AND lower(e.name) = lower(n.name)
     ORDER BY 1;
$$;

REVOKE ALL ON FUNCTION system_rigs_wanted(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_rigs_wanted(text) TO workgraph_app;

COMMIT;
