-- A company-scoped record that is not internal was readable by nobody (WP-H3).
--
-- The read policy from 0008 is
--
--   (scope = 'company' AND visibility = 'internal') OR can_read_project(project_id)
--
-- and can_read_project(NULL) is false, so a company-scoped CONFIDENTIAL or
-- RESTRICTED record satisfies neither branch. It can be written and then read
-- by nobody at all: not the reviewer who accepted it, not the four people who
-- hold organisation-wide access. Durable memory that nothing can retrieve is
-- worse than a gap, because the queue reports it as published.
--
-- It stayed hidden because nothing produced that combination. Every record
-- published so far was company-scoped and internal, or project-scoped. WP-H3
-- asks for a reviewer who can reclassify, and tightening a company-scoped
-- statement to confidential produces exactly this row -- which is how it
-- surfaced: review_candidate uses RETURNING, and RETURNING has to satisfy the
-- read policy, so the acceptance failed outright rather than quietly writing
-- an unreadable record.
--
-- The fix follows the access model already decided rather than inventing one.
-- A record with no project has no project membership to scope it to, so the
-- readers are the company-wide role holders -- the same organisation_admin and
-- executive grants can_read_project honours everywhere (0066, mem-003b900b) --
-- plus the two people the record itself names, its reviewer and its owner.
--
-- Naming those two is not a convenience. Without them an ordinary reviewer
-- tightening a company-scoped statement writes a record they cannot read, and
-- the insert policy then refuses it: the only way to record a sensitive
-- company-wide fact would be to hold a management grant. Somebody may always
-- read what they accepted and what they own.
--
-- Internal stays readable by everyone in the organisation, which is what
-- internal means.
BEGIN;

CREATE OR REPLACE FUNCTION can_read_company_record(
    p_visibility text,
    p_reviewer   uuid,
    p_owner      uuid
) RETURNS boolean AS $$
    SELECT current_app_user() IS NOT NULL
       AND (p_visibility = 'internal'
            OR current_app_user() IN (p_reviewer, p_owner)
            OR EXISTS (
              SELECT 1 FROM role_grants g
               WHERE g.user_id = current_app_user()
                 AND g.project_id IS NULL
                 AND g.role_name IN ('organisation_admin', 'executive')
                 AND (g.expires_at IS NULL OR g.expires_at > now())));
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION can_read_company_record(text, uuid, uuid) IS
  'Who may read a record with no project: everybody when internal, otherwise the company-wide role holders plus the reviewer and owner the record names, because there is no project membership to scope it to (WP-H3).';

-- All three policies carried the same predicate and the same hole. Replaced
-- rather than supplemented: permissive policies OR together, so leaving the
-- old one in place would keep admitting the same rows and the new one would
-- add nothing.
DROP POLICY IF EXISTS knowledge_records_read ON knowledge_records;
CREATE POLICY knowledge_records_read ON knowledge_records FOR SELECT
    USING (CASE WHEN project_id IS NULL
                THEN can_read_company_record(visibility, reviewer_user_id, owner_user_id)
                ELSE can_read_project(project_id) END);

DROP POLICY IF EXISTS knowledge_records_update ON knowledge_records;
CREATE POLICY knowledge_records_update ON knowledge_records FOR UPDATE
    USING (CASE WHEN project_id IS NULL
                THEN can_read_company_record(visibility, reviewer_user_id, owner_user_id)
                ELSE can_read_project(project_id) END)
    -- The row after the update has to be one the writer may still read, or an
    -- update is a way to make a record disappear from its own author.
    WITH CHECK (CASE WHEN project_id IS NULL
                     THEN can_read_company_record(visibility, reviewer_user_id, owner_user_id)
                     ELSE can_read_project(project_id) END);

DROP POLICY IF EXISTS knowledge_records_delete ON knowledge_records;
CREATE POLICY knowledge_records_delete ON knowledge_records FOR DELETE
    USING (CASE WHEN project_id IS NULL
                THEN can_read_company_record(visibility, reviewer_user_id, owner_user_id)
                ELSE can_read_project(project_id) END);

-- The insert policy already required the reviewer to name themselves and to be
-- able to read the project. It gains the same company-scope rule, so a
-- reviewer cannot write a company-scoped record they would not then be allowed
-- to read -- which is what RETURNING would refuse anyway, one step later and
-- with a message about the wrong policy.
DROP POLICY IF EXISTS knowledge_records_insert ON knowledge_records;
CREATE POLICY knowledge_records_insert ON knowledge_records FOR INSERT
    WITH CHECK (
        reviewer_user_id = current_app_user()
        AND CASE WHEN project_id IS NULL
                 THEN can_read_company_record(visibility, reviewer_user_id, owner_user_id)
                 ELSE can_read_project(project_id) END);

COMMIT;
