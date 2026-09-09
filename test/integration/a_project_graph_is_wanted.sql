-- The graph a project should have, and the prefix that must never move.
--
-- Project graphs used to be declared in infra/ansible/group_vars/control.yml,
-- one entry per client engagement. That put client names in a repository that
-- is now public, and meant a project's first accepted candidate was blocked on
-- "no graph for project X" until somebody deployed.
--
-- The prefix is the part worth a test. It appears in every bead id in the graph
-- and a bead id is NEVER rewritten, so:
--
--   a graph that already exists must report its RECORDED prefix, or every id in
--   it is orphaned;
--   a graph that does not must get a derived one, stable and not renumbered.
--
-- The two graphs that predate this were prefixed by hand -- cdt and ngd -- and
-- re-deriving them would produce cdt7 and ngd1, which no existing id matches.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: every read is
-- through a SECURITY DEFINER function that the publisher calls with no app
-- user, and the assertions are about what it returns rather than who may see
-- beads_databases.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  org uuid; u uuid; backup uuid; proj uuid; other uuid; node uuid; cell uuid;
  v_name text; v_prefix text; v_held boolean;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Graph Owner', 'graph-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Graph Backup', 'graph-backup@example.invalid') RETURNING id INTO backup;

  -- A project with a graph already, prefixed by hand the way the real ones are.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id)
  VALUES (org, 'gw-existing', 'Has A Graph', u, backup) RETURNING id INTO proj;
  -- Registered through the function rather than inserted, so this exercises
  -- the path a deploy takes.
  PERFORM system_register_beads_graph('project-gw-existing',
      '/srv/graphs/project-gw-existing', 'probe-host', 'project', 'gw-existing', 'hnd');

  -- And one with none.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id)
  VALUES (org, 'gw-new', 'Needs A Graph', u, backup) RETURNING id INTO other;

  -- The existing one reports what is RECORDED. This is the assertion that
  -- protects every id already in that graph.
  SELECT name, prefix, held INTO v_name, v_prefix, v_held
    FROM system_graph_wanted_for_project('gw-existing');
  IF NOT v_held THEN
      RAISE EXCEPTION 'a project with a registered graph reports held=false, so the '
          'publisher would try to create a second one';
  END IF;
  IF v_prefix <> 'hnd' THEN
      RAISE EXCEPTION 'the recorded prefix reads as %, not hnd; every bead id in that '
          'graph would be orphaned', quote_literal(v_prefix);
  END IF;
  IF v_name <> 'project-gw-existing' THEN
      RAISE EXCEPTION 'the recorded name reads as %', quote_literal(v_name);
  END IF;

  -- The new one gets a derived name and prefix, and is not held.
  SELECT name, prefix, held INTO v_name, v_prefix, v_held
    FROM system_graph_wanted_for_project('gw-new');
  IF v_held THEN
      RAISE EXCEPTION 'a project with no graph reports held=true, so it would stay '
          'blocked for ever';
  END IF;
  IF v_name <> 'project-gw-new' THEN
      RAISE EXCEPTION 'the proposed name is %, not project-gw-new', quote_literal(v_name);
  END IF;
  IF v_prefix IS NULL OR length(v_prefix) < 3 THEN
      RAISE EXCEPTION 'the derived prefix % is too short to be an id prefix', quote_nullable(v_prefix);
  END IF;
  -- Derived from the slug, so it is recognisable rather than opaque.
  IF left(v_prefix, 3) <> 'gwn' THEN
      RAISE EXCEPTION 'the derived prefix % does not come from the slug', quote_literal(v_prefix);
  END IF;

  -- Stable across calls. A prefix that changed between two reads would be a
  -- prefix that changes after beads exist.
  IF v_prefix <> (SELECT prefix FROM system_graph_wanted_for_project('gw-new')) THEN
      RAISE EXCEPTION 'the derived prefix is not stable between calls';
  END IF;

  -- A project that does not exist gets nothing, rather than a graph proposal
  -- for a typo.
  IF EXISTS (SELECT 1 FROM system_graph_wanted_for_project('gw-nope')) THEN
      RAISE EXCEPTION 'a project that does not exist was given a proposed graph';
  END IF;

  -- Re-registering with the SAME prefix is fine; a deploy does it every run.
  PERFORM system_register_beads_graph('project-gw-existing',
      '/srv/graphs/project-gw-existing', 'probe-host', 'project', 'gw-existing', 'hnd');

  -- Omitting it keeps the recorded one. An older deploy document sends no
  -- prefix, and erasing it would be worse than never having recorded it.
  PERFORM system_register_beads_graph('project-gw-existing',
      '/srv/graphs/project-gw-existing', 'probe-host', 'project', 'gw-existing', NULL);
  SELECT prefix INTO v_prefix FROM system_graph_wanted_for_project('gw-existing');
  IF v_prefix <> 'hnd' THEN
      RAISE EXCEPTION 'registering without a prefix erased the recorded one, leaving %',
          quote_nullable(v_prefix);
  END IF;
END
$$;

-- And a CONTRADICTING prefix is refused. This is the assertion that matters
-- most: a graph whose prefix moved is a graph whose every id is wrong, and
-- that must not be discoverable only from a deploy log.
DO $$
BEGIN
  PERFORM system_register_beads_graph('project-gw-existing',
      '/srv/graphs/project-gw-existing', 'probe-host', 'project', 'gw-existing', 'zzz');
  RAISE EXCEPTION 'a graph was re-registered with a different prefix';
EXCEPTION
  WHEN raise_exception THEN
    IF position('ids are never rewritten' IN SQLERRM) = 0 THEN
        RAISE EXCEPTION 'refused for the wrong reason: %', SQLERRM;
    END IF;
END
$$;

ROLLBACK;

SELECT 'an existing graph keeps its prefix, a new one derives a stable prefix' AS result;
