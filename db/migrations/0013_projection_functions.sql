-- 0013_projection_functions.sql — the system path for webhook projections.
-- Work package: WP-D1. Plan section 9.3.

BEGIN;

-- Why these are SECURITY DEFINER.
--
-- Row-level security is built on current_app_user(), and a webhook has no user:
-- GitHub is not a member of a project and cannot be given an identity without
-- inventing one. Running the projection with no identity simply returns nothing,
-- which is how every one of these events was silently recorded as "repository
-- not registered" while the repository was plainly registered.
--
-- The alternative — seeding a system user with membership everywhere — would
-- create an identity that can read every project's work. Anything able to set
-- workgraph.user_id to it would inherit that reach, and RLS would be intact in
-- name only.
--
-- These functions bypass RLS instead, in exactly two places, with fixed SQL and
-- typed parameters. They can insert a projection and nothing else; they cannot
-- be asked to return arbitrary rows. search_path is pinned for the same reason
-- as in 0011: a SECURITY DEFINER function that resolves names through a
-- caller-controlled search_path executes the caller's tables as the owner.

CREATE OR REPLACE FUNCTION project_pull_request(
    p_provider_id text,
    p_owner       text,
    p_name        text,
    p_number      integer,
    p_title       text,
    p_state       text,
    p_author      text,
    p_head_sha    text,
    p_merged_at   timestamptz,
    p_updated_at  timestamptz
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE repo uuid; applied integer;
BEGIN
    -- The provider's numeric ID is preferred because it survives a rename or a
    -- transfer; owner and name are the fallback for a repository that has not
    -- been seen since it was registered.
    SELECT id INTO repo FROM project_repositories
     WHERE provider = 'github'
       AND (provider_id IS NOT NULL AND provider_id = p_provider_id)
     LIMIT 1;

    IF repo IS NULL THEN
        SELECT id INTO repo FROM project_repositories
         WHERE provider = 'github' AND owner = p_owner AND name = p_name
         LIMIT 1;
    END IF;

    IF repo IS NULL THEN
        RETURN NULL;   -- distinct from false: nothing to project against
    END IF;

    -- Record the stable ID the first time we see it, so a later rename cannot
    -- break the link.
    IF p_provider_id IS NOT NULL THEN
        UPDATE project_repositories SET provider_id = p_provider_id
         WHERE id = repo AND provider_id IS DISTINCT FROM p_provider_id;
    END IF;

    -- The WHERE clause on DO UPDATE is the ordering guard. GitHub does not
    -- guarantee delivery order, and a retry of an older event can arrive after
    -- a newer one; without this a redelivered "opened" reopens a merged pull
    -- request. Redelivery is what happens after an outage, so the corruption
    -- would land exactly when the projection was already behind.
    INSERT INTO pull_request_projections
        (repository_id, number, title, state, author, head_sha, merged_at, updated_at)
    VALUES (repo, p_number, p_title, p_state, p_author, p_head_sha, p_merged_at, p_updated_at)
    ON CONFLICT (repository_id, number) DO UPDATE
        SET title      = EXCLUDED.title,
            state      = EXCLUDED.state,
            author     = EXCLUDED.author,
            head_sha   = EXCLUDED.head_sha,
            merged_at  = EXCLUDED.merged_at,
            updated_at = EXCLUDED.updated_at
        WHERE pull_request_projections.updated_at < EXCLUDED.updated_at;

    GET DIAGNOSTICS applied = ROW_COUNT;
    RETURN applied > 0;
END
$$;

CREATE OR REPLACE FUNCTION project_check_suite(
    p_provider_id text,
    p_owner       text,
    p_name        text,
    p_head_sha    text,
    p_state       text
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE repo uuid; applied integer;
BEGIN
    SELECT id INTO repo FROM project_repositories
     WHERE provider = 'github'
       AND (provider_id IS NOT NULL AND provider_id = p_provider_id)
     LIMIT 1;

    IF repo IS NULL THEN
        SELECT id INTO repo FROM project_repositories
         WHERE provider = 'github' AND owner = p_owner AND name = p_name
         LIMIT 1;
    END IF;

    IF repo IS NULL THEN
        RETURN NULL;
    END IF;

    -- Matched on the commit, not the pull request number: a check suite belongs
    -- to a commit, and after a force-push the older suite is no longer about the
    -- code under review. Keying on the commit lets a stale suite match nothing.
    UPDATE pull_request_projections
       SET checks_state = p_state
     WHERE repository_id = repo AND head_sha = p_head_sha;

    GET DIAGNOSTICS applied = ROW_COUNT;
    RETURN applied > 0;
END
$$;

-- Executable by the application role and nobody else. PUBLIC would make the
-- bypass available to every role in the database.
REVOKE ALL ON FUNCTION project_pull_request(text, text, text, integer, text, text, text, text, timestamptz, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION project_check_suite(text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION project_pull_request(text, text, text, integer, text, text, text, text, timestamptz, timestamptz) TO workgraph_app;
GRANT EXECUTE ON FUNCTION project_check_suite(text, text, text, text, text) TO workgraph_app;

COMMIT;
