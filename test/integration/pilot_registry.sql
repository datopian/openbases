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
  -- portaljs-oss and datopian-products came from 0009. nged and cdt are the two
  -- client engagements; cdt was added 2026-09-01 (wg-8yv.37).
  SELECT count(*) INTO n FROM projects
   WHERE slug NOT IN ('portaljs-oss', 'datopian-products', 'nged', 'cdt');
  IF n <> 0 THEN
    RAISE EXCEPTION 'undeclared project(s): %',
      (SELECT string_agg(slug, ', ') FROM projects
        WHERE slug NOT IN ('portaljs-oss', 'datopian-products', 'nged', 'cdt'));
  END IF;

  SELECT count(*) INTO n FROM projects
   WHERE slug IN ('portaljs-oss', 'datopian-products', 'nged', 'cdt');
  IF n <> 4 THEN
    RAISE EXCEPTION 'a declared project is missing: found % of 4', n;
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
           ('nged',              'joao.demenech@datopian.com',     'osahon.okungbowa@datopian.com'),
           ('datopian-products', 'aleksandra.rubaj@datopian.com',  'anuar.ustayev@datopian.com'));
  IF n <> 2 THEN
    RAISE EXCEPTION 'the decided pilot operators are not wired: matched % of 2', n;
  END IF;

  -- projects.*_owner_id records accountability; project_memberships is what
  -- authorisation reads. A project whose accountable operator cannot see it is
  -- the failure that keeping both in step exists to prevent.
  SELECT count(*) INTO n
    FROM projects p
   WHERE p.slug IN ('nged', 'datopian-products')
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

  -- Twelve: the eleven pilot repositories from 0009 plus the agent sandbox
  -- added by 0016. The sandbox is registered rather than special-cased because
  -- the witness routes escalations through this table, and a repository absent
  -- from it produces findings that reach nobody (ADR-0019).
  SELECT count(*) INTO n FROM project_repositories;
  IF n <> 12 THEN RAISE EXCEPTION 'expected 12 repositories, found %', n; END IF;

  -- Named explicitly, because a bare count passes just as happily if the wrong
  -- repository was added.
  IF NOT EXISTS (
      SELECT 1 FROM project_repositories r
        JOIN projects p ON p.id = r.project_id
       WHERE r.owner = 'datopian' AND r.name = 'workgraph-agent-sandbox'
         AND p.slug = 'portaljs-oss') THEN
    RAISE EXCEPTION 'the agent sandbox repository is not registered to portaljs-oss';
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

  RAISE NOTICE 'seed assertions passed';
END
$$;

SET LOCAL ROLE workgraph_app;
-- Prove the drop took effect; see the file for why a convention is not enough.
\ir assert_app_role.sql

DO $$
DECLARE n integer; anu text; osahon text; outsider text;
BEGIN
  SELECT id::text INTO anu      FROM users WHERE primary_email = 'anuar.ustayev@datopian.com';
  SELECT id::text INTO osahon   FROM users WHERE primary_email = 'osahon.okungbowa@datopian.com';
  SELECT id::text INTO outsider FROM users WHERE primary_email = 'outsider@datopian.com';

  -- Anu holds organisation_admin, so the RLS policy lets him see everything.
  -- Note the mechanism: his grant satisfies the policy. The application does
  -- not skip the filter for administrators.
  PERFORM set_config('workgraph.user_id', anu, true);
  SELECT count(*) INTO n FROM projects;
  IF n <> 3 THEN RAISE EXCEPTION 'organisation admin should see 3 projects, saw %', n; END IF;

  -- Osahon leads nged and is a member of nothing else.
  PERFORM set_config('workgraph.user_id', osahon, true);
  SELECT count(*) INTO n FROM projects WHERE slug = 'nged';
  IF n <> 1 THEN RAISE EXCEPTION 'the nged lead cannot see nged'; END IF;
  SELECT count(*) INTO n FROM projects WHERE slug = 'datopian-products';
  IF n <> 0 THEN RAISE EXCEPTION 'the nged lead can see a project they are not a member of'; END IF;

  -- The acceptance criterion. An authenticated non-member must not see the
  -- client engagement at all — not its name, not that it exists.
  PERFORM set_config('workgraph.user_id', outsider, true);
  SELECT count(*) INTO n FROM projects;
  IF n <> 0 THEN RAISE EXCEPTION 'a non-member saw % project(s)', n; END IF;

  SELECT count(*) INTO n FROM projects WHERE slug = 'nged';
  IF n <> 0 THEN RAISE EXCEPTION 'the restricted client project is visible to a non-member'; END IF;

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
