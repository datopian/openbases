-- WP-C3 acceptance, asserted against a real database.
--
-- "create three pilot projects; assign primary and backup operators; attach
-- repositories and cell policy; the restricted project is invisible to
-- unauthorised users."
--
-- Runs after 0009_pilot_seed.sql. Read-only apart from a temporary outsider,
-- and rolls back.

BEGIN;

DO $$
DECLARE n integer; anu uuid; osahon uuid; outsider uuid; org uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  -- ------------------------------------------------------------------
  -- One user per email address, compared the way the login compares it
  -- ------------------------------------------------------------------
  -- internal/domain/store.go binds a Cloudflare Access identity to a user with
  -- `lower(primary_email) = lower($1)`, read with QueryRow -- which takes the
  -- first row and ignores any others. Before 0049 there was no uniqueness at
  -- all, so two rows differing only in case were legal and a first login would
  -- bind the identity to whichever Postgres returned. That is an authorisation
  -- outcome, not a data-tidiness one, which is why it is asserted here.
  BEGIN
    INSERT INTO users (organisation_id, display_name, primary_email, status)
    VALUES (org, 'Case Twin', 'ANUAR.USTAYEV@datopian.com', 'active');
    RAISE EXCEPTION 'a second user was accepted for an address that differs only in case';
  EXCEPTION WHEN unique_violation THEN
    NULL;  -- refused, as intended
  END;

  -- Named rather than counted. A bare count broke the moment a fourth project
  -- was added and said only "found 4", which tells a reader nothing about which
  -- one is unexpected. Naming them keeps the property that mattered -- adding a
  -- project is a deliberate edit here -- and makes the failure legible.
  --
  -- The list lives in ONE place now. It was written out twice, and the copy in
  -- the RAISE had fallen two projects behind the copy in the WHERE, so a
  -- failure named projects that were in fact declared. Two lists that must
  -- agree are one list and a bug waiting.
  --
  -- The shape after the 3 September restructure (0080, 0081):
  --
  --   client    nged, cdt                     the two client engagements
  --   internal  bizdev                        prospect work before it is a client
  --   internal  workgraph                     the platform's own repositories
  --   oss       portaljs, ckan, autoclaw,     one project per repository, split
  --             giftless, wayintoai,          out of portaljs-oss because "the
  --             flowershow                    OSS project" averaged six things
  --   product   datahub, sre                  split out of datopian-products
  --   oss       portaljs-oss                  CLOSED, kept for its 25 attention
  --                                           items rather than deleted
  --
  -- datopian-products and roseville-poc were DELETED by 0080, so they are
  -- deliberately absent rather than forgotten.
  SELECT count(*) INTO n FROM projects
   WHERE slug NOT IN ('nged', 'cdt', 'bizdev', 'workgraph',
                      'portaljs', 'ckan', 'autoclaw', 'giftless', 'wayintoai',
                      'flowershow', 'datahub', 'sre', 'portaljs-oss');
  IF n <> 0 THEN
    RAISE EXCEPTION 'undeclared project(s): %',
      (SELECT string_agg(slug, ', ') FROM projects
        WHERE slug NOT IN ('nged', 'cdt', 'bizdev', 'workgraph',
                           'portaljs', 'ckan', 'autoclaw', 'giftless', 'wayintoai',
                           'flowershow', 'datahub', 'sre', 'portaljs-oss'));
  END IF;

  SELECT count(*) INTO n FROM projects
   WHERE slug IN ('nged', 'cdt', 'bizdev', 'workgraph', 'portaljs', 'ckan',
                  'autoclaw', 'giftless', 'wayintoai', 'flowershow',
                  'datahub', 'sre', 'portaljs-oss');
  IF n <> 13 THEN
    RAISE EXCEPTION 'a declared project is missing: found % of 13', n;
  END IF;

  -- ------------------------------------------------------------------
  -- The operators decided in wg-8yv.37 are the ones actually wired
  -- ------------------------------------------------------------------
  -- Asserted by email, because the address is the Cloudflare Access identity
  -- and a display name is not a key.
  SELECT count(*) INTO n
    FROM projects p
    JOIN users po ON po.id = p.primary_owner_id
    JOIN users bo ON bo.id = p.backup_owner_id
   WHERE (p.slug, po.primary_email, bo.primary_email) IN (
           ('nged',    'joao.demenech@datopian.com',    'osahon.okungbowa@datopian.com'),
           -- datopian-products was dissolved by 0080, and the decision followed
           -- the work rather than the container: datahub and sre both carry
           -- aleksandra and ANUAR. Every other project here has osahon as
           -- backup, so a reorganisation that defaulted to the house convention
           -- would have overwritten a recorded decision without saying so.
           ('datahub', 'aleksandra.rubaj@datopian.com', 'anuar.ustayev@datopian.com'),
           ('sre',     'aleksandra.rubaj@datopian.com', 'anuar.ustayev@datopian.com'));
  IF n <> 3 THEN
    RAISE EXCEPTION 'the decided pilot operators are not wired: matched % of 3', n;
  END IF;

  -- projects.*_owner_id records accountability; project_memberships is what
  -- authorisation reads. A project whose accountable operator cannot see it is
  -- the failure that keeping both in step exists to prevent.
  SELECT count(*) INTO n
    FROM projects p
   WHERE p.slug IN ('nged', 'datahub', 'sre')
     AND (NOT EXISTS (SELECT 1 FROM project_memberships m
                       WHERE m.project_id = p.id AND m.user_id = p.primary_owner_id
                         AND m.role_name = 'project_lead')
       OR NOT EXISTS (SELECT 1 FROM project_memberships m
                       WHERE m.project_id = p.id AND m.user_id = p.backup_owner_id
                         AND m.role_name = 'backup_operator'));
  IF n <> 0 THEN
    RAISE EXCEPTION '% project(s) where the owner columns and the memberships disagree', n;
  END IF;

  -- Every project has two DIFFERENT accountable people. The schema enforces
  -- it; this proves the seed actually exercised that rather than sidestepping.
  SELECT count(*) INTO n FROM projects WHERE primary_owner_id = backup_owner_id;
  IF n <> 0 THEN RAISE EXCEPTION '% project(s) have the same primary and backup owner', n; END IF;

  -- Thirteen: the eleven pilot repositories from 0009, the agent sandbox added
  -- by 0016, and datopian/data-portal-examples registered to bizdev by 0080.
  -- The sandbox is registered rather than special-cased because the witness
  -- routes escalations through this table, and a repository absent from it
  -- produces findings that reach nobody (ADR-0019).
  --
  -- A count rather than names, deliberately: what matters here is that the
  -- restructure MOVED repositories between projects and did not create or lose
  -- any. Which project holds which is asserted below.
  SELECT count(*) INTO n FROM project_repositories;
  IF n <> 13 THEN RAISE EXCEPTION 'expected 13 repositories, found %', n; END IF;

  -- Where the restructure put them (0080). Named, because a count would pass
  -- if two repositories swapped projects -- and "which project owns this
  -- repository" decides who sees its pull requests.
  SELECT count(*) INTO n
    FROM project_repositories r
    JOIN projects p ON p.id = r.project_id
   WHERE (p.slug, r.owner, r.name) IN (
           ('bizdev',     'datopian',   'data-portal-examples'),
           ('workgraph',  'datopian',   'workgraph-agent-sandbox'),
           ('portaljs',   'datopian',   'portaljs'),
           ('portaljs',   'datopian',   'cloud.portaljs.com'),
           ('ckan',       'ckan',       'ckan'),
           ('autoclaw',   'datopian',   'autoclaw.sh'),
           ('giftless',   'datopian',   'giftless'),
           ('wayintoai',  'datopian',   'wayintoai'),
           ('flowershow', 'flowershow', 'flowershow'),
           ('datahub',    'datopian',   'datahub-next'),
           ('datahub',    'datopian',   'postal-codes'),
           ('sre',        'datopian',   'sre-agent'),
           ('nged',       'datopian',   'nged'));
  IF n <> 13 THEN
    RAISE EXCEPTION 'the restructure left repositories on the wrong project: matched % of 13', n;
  END IF;

  -- portaljs-oss is closed and must hold nothing. A closed project still
  -- holding repositories is the half-finished state 0081 had to correct for
  -- work references.
  SELECT count(*) INTO n
    FROM project_repositories r
    JOIN projects p ON p.id = r.project_id
   WHERE p.slug = 'portaljs-oss';
  IF n <> 0 THEN
    RAISE EXCEPTION 'portaljs-oss is closed and still holds % repositories', n;
  END IF;

  -- Named explicitly, because a bare count passes just as happily if the wrong
  -- repository was added.
  --
  -- The sandbox moved to the internal `workgraph` project in 0080. It was on
  -- portaljs-oss only because 0016 had nowhere better to put it, and it is not
  -- open-source work: it is the rig the platform runs its own agents in. What
  -- this assertion is really about is unchanged -- the sandbox must be
  -- REGISTERED somewhere, because the witness routes escalations through this
  -- table and a repository absent from it produces findings that reach nobody
  -- (ADR-0019).
  IF NOT EXISTS (
      SELECT 1 FROM project_repositories r
        JOIN projects p ON p.id = r.project_id
       WHERE r.owner = 'datopian' AND r.name = 'workgraph-agent-sandbox'
         AND p.slug = 'workgraph') THEN
    RAISE EXCEPTION 'the agent sandbox repository is not registered to the workgraph project';
  END IF;

  -- Owners must be members: the RLS policy checks membership, not ownership,
  -- so an owner who is not a member cannot read their own project.
  SELECT count(*) INTO n
  FROM projects p
  WHERE NOT EXISTS (SELECT 1 FROM project_memberships m
                    WHERE m.project_id = p.id AND m.user_id = p.primary_owner_id);
  IF n <> 0 THEN RAISE EXCEPTION '% project(s) have a primary owner who is not a member', n; END IF;

  SELECT id INTO anu    FROM users WHERE primary_email = 'anuar.ustayev@datopian.com';
  SELECT id INTO osahon FROM users WHERE primary_email = 'osahon.okungbowa@datopian.com';

  -- Somebody authenticated but a member of nothing, which is the shape of a
  -- new joiner or a marketing user with no project grants.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Outsider', 'outsider@datopian.com')
  RETURNING id INTO outsider;

  -- Stashed now, because it cannot be looked up later. The block below runs as
  -- workgraph_app, where `projects` is behind row-level security and no identity
  -- is set yet -- so resolving the lead there returns NULL and every assertion
  -- about them then fails for the wrong reason. (It did.)
  PERFORM set_config('test.lead_nged',
                     (SELECT primary_owner_id::text FROM projects WHERE slug = 'nged'),
                     false);

  RAISE NOTICE 'seed assertions passed';
END
$$;

SET LOCAL ROLE workgraph_app;
-- Prove the drop took effect; see the file for why a convention is not enough.
\ir assert_app_role.sql

DO $$
DECLARE n integer; anu text; lead_nged text; outsider text;
BEGIN
  SELECT id::text INTO anu       FROM users WHERE primary_email = 'anuar.ustayev@datopian.com';
  SELECT id::text INTO outsider  FROM users WHERE primary_email = 'outsider@datopian.com';
  -- Read from the setting stashed above rather than named. The lead used to be
  -- Osahon and is now Demenech (wg-8yv.37); a test that hard-codes the person
  -- asserts an org chart rather than a policy.
  lead_nged := current_setting('test.lead_nged', true);
  IF lead_nged IS NULL OR lead_nged = '' THEN
    RAISE EXCEPTION 'the nged lead was not stashed before the role drop';
  END IF;

  -- Anu holds organisation_admin, so the RLS policy lets him see everything.
  -- Note the mechanism: his grant satisfies the policy. The application does
  -- not skip the filter for administrators.
  -- Named, not counted. The literal was 3 and it failed the moment a fourth
  -- project was added -- a test breaking on an unrelated change rather than on
  -- the property it checks. The property is "an admin sees every declared
  -- project", and naming them says that without needing a total to compare to.
  PERFORM set_config('workgraph.user_id', anu, true);
  SELECT count(*) INTO n FROM projects
   WHERE slug IN ('nged', 'cdt', 'bizdev', 'workgraph', 'portaljs', 'ckan',
                  'autoclaw', 'giftless', 'wayintoai', 'flowershow',
                  'datahub', 'sre', 'portaljs-oss');
  IF n <> 13 THEN
    RAISE EXCEPTION 'organisation admin saw % of the 13 declared projects', n;
  END IF;

  -- The nged lead sees nged and not a project they are not a member of.
  PERFORM set_config('workgraph.user_id', lead_nged, true);
  SELECT count(*) INTO n FROM projects WHERE slug = 'nged';
  IF n <> 1 THEN RAISE EXCEPTION 'the nged lead cannot see nged'; END IF;
  -- datahub rather than datopian-products, which 0080 deleted. Any project the
  -- nged lead is not a member of serves; this one because it is in a different
  -- portfolio, so the assertion is not accidentally about client isolation,
  -- which the cdt check below covers deliberately.
  SELECT count(*) INTO n FROM projects WHERE slug = 'datahub';
  IF n <> 0 THEN RAISE EXCEPTION 'the nged lead can see a project they are not a member of'; END IF;

  -- And not the OTHER client engagement. Two restricted client projects now
  -- exist, so "a client project is invisible to non-members" has to hold
  -- between them, not just against an outsider -- that is the case a single
  -- client project could never test.
  SELECT count(*) INTO n FROM projects WHERE slug = 'cdt';
  IF n <> 0 THEN
    RAISE EXCEPTION 'the nged lead can see the cdt client engagement';
  END IF;

  -- The acceptance criterion. An authenticated non-member must not see the
  -- client engagement at all — not its name, not that it exists.
  PERFORM set_config('workgraph.user_id', outsider, true);
  SELECT count(*) INTO n FROM projects;
  IF n <> 0 THEN RAISE EXCEPTION 'a non-member saw % project(s)', n; END IF;

  SELECT count(*) INTO n FROM projects WHERE slug IN ('nged', 'cdt');
  IF n <> 0 THEN RAISE EXCEPTION 'a restricted client project is visible to a non-member'; END IF;

  -- Its repositories name the client too, so they must be unreachable as well.
  SELECT count(*) INTO n FROM project_repositories;
  IF n <> 0 THEN RAISE EXCEPTION 'a non-member saw % repositories', n; END IF;

  -- And with no identity at all.
  PERFORM set_config('workgraph.user_id', '', true);
  SELECT count(*) INTO n FROM projects;
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified session saw % project(s)', n; END IF;

  RAISE NOTICE 'isolation assertions passed';
END
$$;

RESET ROLE;
ROLLBACK;
