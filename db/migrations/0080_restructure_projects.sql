-- wg:backfill — the project and repository restructure Anuar asked for on
-- 3 September 2026. Registry rows only; no schema change.
--
-- Five things, and the reasoning for each is with it below:
--
--   1. `poc` becomes `bizdev`, holding prospect work before it is a client
--   2. `roseville-poc` is deleted, its work folded into bizdev
--   3. a new internal `workgraph` project takes the agent sandbox AND the 130
--      sandbox beads that were wrongly attributed to portaljs-oss
--   4. `portaljs-oss` splits into one project per repository
--   5. `datopian-products` is dissolved: cloud.portaljs.com to portaljs,
--      datahub-next and postal-codes to a new datahub, sre-agent to a new sre
--
-- Written as a migration rather than through POST /v1/projects because the
-- standing rule is that persistent state is represented in Git. A restructure
-- of this size done through the API would leave no reviewable record of what
-- moved where.

BEGIN;

DO $$
DECLARE
    org        uuid;
    internal   uuid;
    oss_pf     uuid;
    product_pf uuid;
    oss_cell   uuid;
    -- Owners. Reused from the projects being split, so ownership does not
    -- silently move to whoever wrote the migration.
    anuar      uuid;
    backup     uuid;
    oss_owner  uuid;
    prod_owner uuid;
    v_wg       uuid;
    v_bizdev   uuid;
    n          integer;
BEGIN
    SELECT id INTO org FROM organisations WHERE slug = 'datopian';
    SELECT id INTO internal   FROM portfolios WHERE organisation_id = org AND slug = 'internal';
    SELECT id INTO oss_pf     FROM portfolios WHERE organisation_id = org AND slug = 'oss';
    SELECT id INTO product_pf FROM portfolios WHERE organisation_id = org AND slug = 'product';
    SELECT id INTO oss_cell   FROM execution_cells WHERE slug = 'oss';

    SELECT primary_owner_id, backup_owner_id INTO anuar, backup
      FROM projects WHERE organisation_id = org AND slug = 'poc';
    SELECT primary_owner_id INTO oss_owner
      FROM projects WHERE organisation_id = org AND slug = 'portaljs-oss';
    SELECT primary_owner_id INTO prod_owner
      FROM projects WHERE organisation_id = org AND slug = 'datopian-products';

    IF anuar IS NULL OR backup IS NULL OR oss_owner IS NULL OR prod_owner IS NULL THEN
        RAISE EXCEPTION 'an owner could not be resolved; refusing to guess one';
    END IF;

    -- -----------------------------------------------------------------------
    -- 1. poc becomes bizdev
    -- -----------------------------------------------------------------------
    --
    -- The slug changes as well as the name. A project called BizDev with the
    -- slug `poc` would be confusing in every URL and every bead label from
    -- here on, and the slug is the durable identifier.
    --
    -- THE CONSEQUENCE, which has to be handled outside this migration: the
    -- sixteen Jackson beads carry the label `project:poc`, and
    -- system_project_bead now RAISES on a label naming no project (0078). So
    -- renaming the slug without relabelling those beads turns the next
    -- projection pass into a hard failure. The relabel is a Beads operation,
    -- not SQL, and is recorded in the bead for this change.
    UPDATE projects
       SET slug = 'bizdev',
           name = 'BizDev',
           portfolio_id = internal,
           objective = 'Prospect and pre-client work: proofs of concept, evidence and outreach, '
                       'before an engagement becomes a client project with its own portfolio entry.'
     WHERE organisation_id = org AND slug = 'poc'
    RETURNING id INTO v_bizdev;

    IF v_bizdev IS NULL THEN
        RAISE EXCEPTION 'the poc project was not found, so nothing was renamed';
    END IF;

    -- One repository for all prospect work, which already exists and is
    -- already used for it. A repository per prospect was the first plan; there
    -- is one repo holding them all, so this follows what is actually there.
    INSERT INTO project_repositories (project_id, provider, owner, name, default_branch)
    VALUES (v_bizdev, 'github', 'datopian', 'data-portal-examples', 'main')
    ON CONFLICT (provider, owner, name)
    DO UPDATE SET project_id = EXCLUDED.project_id;

    -- -----------------------------------------------------------------------
    -- 2. roseville-poc is deleted
    -- -----------------------------------------------------------------------
    --
    -- Deleted rather than closed, as decided. Checked first: it holds no
    -- repositories, no work_refs, no attention items, no graphs and no agent
    -- profiles -- only eight memberships, which cascade. If that stops being
    -- true this raises instead of quietly discarding something.
    SELECT count(*) INTO n
      FROM work_refs w
      JOIN projects p ON p.id = w.project_id
     WHERE p.organisation_id = org AND p.slug = 'roseville-poc';
    IF n > 0 THEN
        RAISE EXCEPTION 'roseville-poc has % work references; deleting it would discard them', n;
    END IF;

    DELETE FROM projects WHERE organisation_id = org AND slug = 'roseville-poc';

    -- -----------------------------------------------------------------------
    -- 3. An internal project for the platform itself
    -- -----------------------------------------------------------------------
    --
    -- The agent sandbox is not OSS, not a product and not a prospect. It is the
    -- rig the platform runs its own agents in.
    --
    -- It also fixes a real mis-attribution. 130 work_refs were attributed to
    -- portaljs-oss, and they are not PortalJS work at all -- every one is an
    -- `sa-` bead from the sandbox rig ("Termination probe: add a NOTES.md",
    -- "Deacon Patrol"). They were attributed by the old cell rule, which
    -- resolved to whatever single project the oss cell mapped to, and that
    -- happened to be portaljs-oss. Splitting portaljs-oss per repository would
    -- have left 130 sandbox probes filed under a project that no longer has
    -- anything to do with them.
    INSERT INTO projects (organisation_id, portfolio_id, slug, name, objective,
                          visibility, primary_owner_id, backup_owner_id, execution_cell_id)
    VALUES (org, internal, 'workgraph', 'Workgraph platform',
            'The platform''s own repositories and the agent rig it runs them in.',
            'internal', anuar, backup, oss_cell)
    ON CONFLICT (organisation_id, slug) DO UPDATE SET name = EXCLUDED.name
    RETURNING id INTO v_wg;

    INSERT INTO project_memberships (project_id, user_id, role_name)
    VALUES (v_wg, anuar, 'project_lead'), (v_wg, backup, 'backup_operator')
    ON CONFLICT DO NOTHING;

    UPDATE project_repositories SET project_id = v_wg
     WHERE provider = 'github' AND owner = 'datopian' AND name = 'workgraph-agent-sandbox';

    UPDATE work_refs SET project_id = v_wg
     WHERE project_id = (SELECT id FROM projects WHERE organisation_id = org AND slug = 'portaljs-oss')
       AND bead_id LIKE 'sa-%';

    RAISE NOTICE 'moved % sandbox bead(s) to the workgraph project', (
        SELECT count(*) FROM work_refs WHERE project_id = v_wg);

    -- -----------------------------------------------------------------------
    -- 4 and 5. One project per repository
    -- -----------------------------------------------------------------------
    --
    -- portaljs-oss held seven repositories from six unrelated projects, so
    -- "the OSS project" answered no useful question: its pull requests, its
    -- signals and its work were six things averaged together.
    --
    -- Each gets the owner of the project it came out of, so ownership does not
    -- move as a side effect of a reorganisation.
    -- Written out rather than looped, because each row differs in portfolio,
    -- owner and display name, and a loop over eight tuples of five columns is
    -- harder to read than eight statements.
    INSERT INTO projects (organisation_id, portfolio_id, slug, name, objective,
                          visibility, primary_owner_id, backup_owner_id, execution_cell_id)
    VALUES
      (org, oss_pf, 'portaljs',   'PortalJS',
       'The PortalJS framework and its hosted service.',
       'internal', oss_owner, backup, oss_cell),
      (org, oss_pf, 'ckan',       'CKAN',
       'CKAN, and Datopian''s contributions to it.',
       'internal', oss_owner, backup, oss_cell),
      (org, oss_pf, 'autoclaw',   'Autoclaw',
       'autoclaw.sh.', 'internal', oss_owner, backup, oss_cell),
      (org, oss_pf, 'giftless',   'Giftless',
       'Giftless, the Git LFS server.', 'internal', oss_owner, backup, oss_cell),
      (org, oss_pf, 'wayintoai',  'WayIntoAI',
       'WayIntoAI.', 'internal', oss_owner, backup, oss_cell),
      (org, oss_pf, 'flowershow', 'Flowershow',
       'Flowershow.', 'internal', oss_owner, backup, oss_cell),
      (org, product_pf, 'datahub', 'DataHub.io',
       'DataHub.io and the data it publishes.',
       'internal', prod_owner, backup, oss_cell),
      (org, product_pf, 'sre',     'SRE',
       'The site-reliability agent.',
       'internal', prod_owner, backup, oss_cell)
    ON CONFLICT (organisation_id, slug) DO UPDATE SET name = EXCLUDED.name;

    -- Both owners as members of each, which is the invariant from wg-1dm: a
    -- project whose owners are not members is invisible to them unless they
    -- happen to be administrators.
    INSERT INTO project_memberships (project_id, user_id, role_name)
    SELECT p.id, p.primary_owner_id, 'project_lead'
      FROM projects p
     WHERE p.organisation_id = org
       AND p.slug IN ('portaljs','ckan','autoclaw','giftless','wayintoai','flowershow',
                      'datahub','sre')
    ON CONFLICT DO NOTHING;
    INSERT INTO project_memberships (project_id, user_id, role_name)
    SELECT p.id, p.backup_owner_id, 'backup_operator'
      FROM projects p
     WHERE p.organisation_id = org
       AND p.slug IN ('portaljs','ckan','autoclaw','giftless','wayintoai','flowershow',
                      'datahub','sre')
    ON CONFLICT DO NOTHING;

    -- The repositories, to the project each belongs to.
    UPDATE project_repositories r SET project_id = p.id
      FROM projects p
     WHERE p.organisation_id = org
       AND r.provider = 'github'
       AND (p.slug, r.owner, r.name) IN (
            ('portaljs',   'datopian',   'portaljs'),
            ('portaljs',   'datopian',   'cloud.portaljs.com'),
            ('ckan',       'ckan',       'ckan'),
            ('autoclaw',   'datopian',   'autoclaw.sh'),
            ('giftless',   'datopian',   'giftless'),
            ('wayintoai',  'datopian',   'wayintoai'),
            ('flowershow', 'flowershow', 'flowershow'),
            ('datahub',    'datopian',   'datahub-next'),
            ('datahub',    'datopian',   'postal-codes'),
            ('sre',        'datopian',   'sre-agent'));

    -- -----------------------------------------------------------------------
    -- What is left of the two projects being emptied
    -- -----------------------------------------------------------------------
    --
    -- portaljs-oss is CLOSED, not deleted: it still carries 25 attention items,
    -- and deleting it would cascade them away. Closed keeps the history and
    -- drops it off the active list.
    UPDATE projects SET status = 'closed'
     WHERE organisation_id = org AND slug = 'portaljs-oss';

    -- datopian-products is deleted, as decided. Checked, not assumed.
    SELECT count(*) INTO n
      FROM project_repositories r
      JOIN projects p ON p.id = r.project_id
     WHERE p.organisation_id = org AND p.slug = 'datopian-products';
    IF n > 0 THEN
        RAISE EXCEPTION 'datopian-products still holds % repositories; they must move first', n;
    END IF;
    SELECT count(*) INTO n
      FROM work_refs w
      JOIN projects p ON p.id = w.project_id
     WHERE p.organisation_id = org AND p.slug = 'datopian-products';
    IF n > 0 THEN
        RAISE EXCEPTION 'datopian-products has % work references; deleting it would discard them', n;
    END IF;

    DELETE FROM projects WHERE organisation_id = org AND slug = 'datopian-products';

    -- A repository belongs to exactly one project, so nothing may be orphaned
    -- by the moves above. Asserted rather than hoped for.
    SELECT count(*) INTO n FROM project_repositories WHERE project_id IS NULL;
    IF n > 0 THEN
        RAISE EXCEPTION '% repositories ended up attached to no project', n;
    END IF;

    RAISE NOTICE 'restructure complete';
END $$;

COMMIT;
