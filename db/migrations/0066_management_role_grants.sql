-- The management role grants, in Git (wg-p4h.12).
--
-- Three of the six company-wide grants existed only on staging. They were a
-- deliberate decision, applied through the audited path on 2026-08-30 and
-- recorded in company-workgraph -- and they were in no migration, so a
-- database rebuilt from this directory had two company-wide readers where
-- staging has four.
--
-- That is exactly the drift the go-live rule exists to prevent: an access
-- decision that survives only in one running database. It showed up as a test
-- passing against staging and failing in CI, which is the cheap way to find
-- out; the expensive way is a restore drill that quietly returns a different
-- permission model.
--
-- The decision itself is unchanged and is not being re-litigated here. From the
-- wg-p4h.12 record: anuar holds executive alongside organisation_admin, daniela
-- holds organisation_admin alongside function_lead, osahon holds
-- organisation_admin, rufus keeps executive. Its consequence is also recorded
-- and still true -- role grants are additive, so holding organisation_admin and
-- executive together reconstitutes the combination ADR-0026's matrix separates,
-- and nothing checks the combination.
--
-- organisation_admin and executive are the two roles can_read_project honours
-- company-wide, so these grants ARE the "management reads every project"
-- policy. test/integration/management_access.sql asserts it.
BEGIN;

-- Idempotent, and it does not touch a grant that already exists: the rows on
-- staging carry a granted_by from the person who applied them, and rewriting
-- them from a migration would replace an attributed grant with an anonymous
-- one.
INSERT INTO role_grants (user_id, role_name, organisation_id)
SELECT u.id, v.role_name, o.id
FROM organisations o
JOIN users u ON u.organisation_id = o.id
JOIN (VALUES
        ('anuar.ustayev@datopian.com', 'executive'),
        ('daniela.popova@datopian.com', 'organisation_admin'),
        ('osahon.okungbowa@datopian.com', 'organisation_admin')
     ) AS v(email, role_name) ON v.email = u.primary_email
WHERE o.slug = 'datopian'
ON CONFLICT DO NOTHING;

COMMIT;
