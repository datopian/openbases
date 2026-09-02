-- work_links had no row-level security at all (WP-D2).
--
-- Not enabled, not forced, no policies. Every other project-scoped table was
-- covered by 0008 or 0010, and this one was created in 0001 alongside them and
-- missed by both -- 0010 sweeps tables carrying a project_id column, and a
-- cross-graph edge carries none: it names two work_refs, and the project lives
-- on those.
--
-- What that allows is worse than a read leak. With no policy there is no
-- restriction on INSERT or DELETE either, so any authenticated caller can
-- assert an edge between any two beads, or remove one. work_links holds
-- 'evidences' and 'supersedes' edges, which is audit history; deleting those is
-- tampering, and the table permitted it.
--
-- The rule is the only one that makes sense for an edge: you may see it if you
-- may see BOTH beads it connects, and you may write it under the same
-- condition. Anything looser leaks the existence of a relationship between two
-- client projects to somebody entitled to neither, which is the shape of leak
-- that matters here -- not the content of the work, but who is working with
-- whom.
BEGIN;

ALTER TABLE work_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_links FORCE ROW LEVEL SECURITY;

-- Set-based rather than a function call per row, following 0023: the endpoints
-- are resolved through work_refs' own visibility, so the predicate is a
-- semi-join the planner can hash once instead of a correlated EXISTS it
-- re-executes per edge.
--
-- Spelled out twice rather than factored into a function, for the reason 0023
-- records at length: inside a function the subquery becomes a SubPlan
-- re-executed per call and the predicate never reaches the planner, which cost
-- 18x there.
CREATE POLICY work_links_read ON work_links FOR SELECT
    USING (
        from_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL
                OR w.project_id IN (
                     SELECT m.project_id FROM project_memberships m
                      WHERE m.user_id = current_app_user())
                OR EXISTS (
                     SELECT 1 FROM role_grants g
                      WHERE g.user_id = current_app_user()
                        AND g.project_id IS NULL
                        AND g.role_name IN ('organisation_admin', 'executive')
                        AND (g.expires_at IS NULL OR g.expires_at > now())))
        AND to_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL
                OR w.project_id IN (
                     SELECT m.project_id FROM project_memberships m
                      WHERE m.user_id = current_app_user())
                OR EXISTS (
                     SELECT 1 FROM role_grants g
                      WHERE g.user_id = current_app_user()
                        AND g.project_id IS NULL
                        AND g.role_name IN ('organisation_admin', 'executive')
                        AND (g.expires_at IS NULL OR g.expires_at > now()))));

-- Writes require an identified caller as well as both endpoints, and the
-- creator must be themselves. Separate policies per command rather than FOR
-- ALL: 0008 records that a FOR ALL write policy also covers SELECT and would
-- silently re-open every read, which is exactly what rls_isolation.sql caught
-- once already.
CREATE POLICY work_links_insert ON work_links FOR INSERT
    WITH CHECK (
        current_app_user() IS NOT NULL
        AND (created_by IS NULL OR created_by = current_app_user())
        AND from_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL OR can_read_project(w.project_id))
        AND to_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL OR can_read_project(w.project_id)));

-- No UPDATE policy: an edge has no mutable field worth changing. Its shape is
-- (from, to, relation), and changing any of those is a different edge --
-- withdraw one and assert the other, so the history says what happened.
CREATE POLICY work_links_delete ON work_links FOR DELETE
    USING (
        current_app_user() IS NOT NULL
        AND from_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL OR can_read_project(w.project_id))
        AND to_work_ref IN (
            SELECT w.id FROM work_refs w
             WHERE w.project_id IS NULL OR can_read_project(w.project_id)));

-- Widening the structural guard to tables keyed on work_refs found two more
-- with no row-level security at all. Both are here because they are the same
-- omission with the same cause, and a guard that fails on the real tree is not
-- a guard.

-- context_pack_items: the pack row was protected and its CONTENTS were not.
--
-- A context pack is permission-filtered by construction and holds exactly what
-- one principal was shown, item by item, with the reason each was included.
-- Protecting the pack and leaving the items open defeats the point of building
-- the pack that way.
--
-- The rule delegates rather than restating: an item is visible when its pack
-- is. Restating context_packs' predicate here would be a second copy free to
-- drift from the one that matters.
ALTER TABLE context_pack_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE context_pack_items FORCE ROW LEVEL SECURITY;

CREATE POLICY context_pack_items_read ON context_pack_items FOR SELECT
    USING (context_pack_id IN (SELECT id FROM context_packs));

CREATE POLICY context_pack_items_insert ON context_pack_items FOR INSERT
    WITH CHECK (context_pack_id IN (SELECT id FROM context_packs));

CREATE POLICY context_pack_items_delete ON context_pack_items FOR DELETE
    USING (context_pack_id IN (SELECT id FROM context_packs));

-- improvement_proposals: readable by anybody in the organisation, because the
-- improvement loop is company-visible work by design (plan section 14.8). The
-- gap was in the writes.
--
-- The CHECK constraints already refuse self-approval and an unapproved
-- 'approved' row, so the governance rule from ADR-0014 was never at risk. What
-- was missing is that nothing tied a row to its writer: any caller could file
-- a proposal under somebody else's name, or record somebody else's approval.
-- The constraints then compare two names neither of which the writer owns.
ALTER TABLE improvement_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE improvement_proposals FORCE ROW LEVEL SECURITY;

CREATE POLICY improvement_proposals_read ON improvement_proposals FOR SELECT
    USING (current_app_user() IS NOT NULL);

CREATE POLICY improvement_proposals_insert ON improvement_proposals FOR INSERT
    WITH CHECK (
        current_app_user() IS NOT NULL
        -- An agent-proposed row names an agent and no user, which is the other
        -- half of proposal_has_proposer; a human-proposed one must name the
        -- caller.
        AND (proposed_by_user_id IS NULL OR proposed_by_user_id = current_app_user())
        -- A proposal cannot arrive pre-approved by somebody else. Combined with
        -- no_self_approval, approval is then always somebody else's separate
        -- act, which is what ADR-0014 asks for.
        AND (approved_by_user_id IS NULL OR approved_by_user_id = current_app_user()));

CREATE POLICY improvement_proposals_update ON improvement_proposals FOR UPDATE
    USING (current_app_user() IS NOT NULL)
    WITH CHECK (
        current_app_user() IS NOT NULL
        AND (approved_by_user_id IS NULL OR approved_by_user_id = current_app_user()));

COMMIT;
