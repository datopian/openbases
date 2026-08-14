-- 0014_projection_rls_and_system_reads.sql
-- Close the projection read leak, and give the system path explicit reads.
-- Work package: WP-D3. Plan sections 8.4, 18.

BEGIN;

-- pull_request_projections carried no row-level security.
--
-- It holds pull request titles, authors and branch names for every registered
-- repository, including the restricted client work in nged. That is the same
-- leak as project_repositories in 0010, in a table added afterwards — which is
-- the argument for the structural guard rather than for remembering.
ALTER TABLE pull_request_projections ENABLE ROW LEVEL SECURITY;
ALTER TABLE pull_request_projections FORCE ROW LEVEL SECURITY;

-- Visibility follows the repository's project, because that is where the
-- visibility class lives. A projection is exactly as sensitive as the work it
-- describes.
CREATE POLICY pull_request_projections_read ON pull_request_projections
    FOR SELECT USING (
        EXISTS (
            SELECT 1 FROM project_repositories r
             WHERE r.id = pull_request_projections.repository_id
               AND can_read_project(r.project_id)
        )
    );

-- No INSERT, UPDATE or DELETE policy is defined, so no user session may write
-- one directly. Projections arrive only through the SECURITY DEFINER functions
-- in 0013, which is the sole path that has been reviewed for ordering and for
-- merged-versus-closed.

-- github_deliveries holds raw GitHub payloads: titles, branch names and commit
-- messages from every installed repository, restricted ones included. No user
-- session has a reason to read them, so none may.
ALTER TABLE github_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_deliveries FORCE ROW LEVEL SECURITY;

-- Deliberately no policy at all. With row-level security enabled and no
-- permissive policy, every ordinary session sees nothing; the system path below
-- reaches them as the owner.

-- ---------------------------------------------------------------------------
-- The system path
-- ---------------------------------------------------------------------------
--
-- Reconciliation runs on a timer with no user, exactly like the webhook. The
-- same reasoning as 0013 applies: rather than invent an identity that can read
-- everything, the system path gets narrow SECURITY DEFINER functions that
-- return only what reconciliation needs.
--
-- This was found the honest way. The first reconciliation pass reported zero
-- repositories resynced and zero failures — a silent no-op, because
-- project_repositories is protected and the pass had no identity.

CREATE OR REPLACE FUNCTION system_github_repositories()
RETURNS TABLE (owner text, name text, provider_id text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT r.owner, r.name, COALESCE(r.provider_id, '')
      FROM project_repositories r
     WHERE r.provider = 'github'
     ORDER BY r.owner, r.name;
$$;

CREATE OR REPLACE FUNCTION system_pending_deliveries(p_limit integer)
RETURNS TABLE (delivery_id text, event_type text, payload jsonb)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT d.delivery_id, d.event_type, d.payload
      FROM github_deliveries d
     WHERE d.processed_at IS NULL
     ORDER BY d.received_at
     LIMIT p_limit;
$$;

CREATE OR REPLACE FUNCTION system_mark_delivery_processed(p_delivery_id text)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE github_deliveries SET processed_at = now()
     WHERE delivery_id = p_delivery_id;
$$;

-- The webhook path records receipts, so it needs the write side too.
CREATE OR REPLACE FUNCTION system_record_delivery(
    p_delivery_id text,
    p_event_type  text,
    p_payload     jsonb
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE inserted integer;
BEGIN
    INSERT INTO github_deliveries (delivery_id, event_type, payload)
    VALUES (p_delivery_id, p_event_type, p_payload)
    ON CONFLICT (delivery_id) DO NOTHING;
    GET DIAGNOSTICS inserted = ROW_COUNT;
    -- false means the delivery was already recorded: a retry, not a failure.
    RETURN inserted > 0;
END
$$;

REVOKE ALL ON FUNCTION system_github_repositories() FROM PUBLIC;
REVOKE ALL ON FUNCTION system_pending_deliveries(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_mark_delivery_processed(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_record_delivery(text, text, jsonb) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION system_github_repositories() TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_pending_deliveries(integer) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_mark_delivery_processed(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_record_delivery(text, text, jsonb) TO workgraph_app;

COMMIT;
