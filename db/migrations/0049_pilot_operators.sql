-- wg:backfill — adds the three pilot operators and rewires two projects.
--
-- The three pilot projects and their operators (wg-8yv.37), and the named people
-- behind them (wg-8yv.39). Decided by Anuar 2026-09-01:
--
--   CDT                 Monika Popova      backup Osahon Okungbowa
--   NGED                Joao Demenech      backup Osahon Okungbowa
--   DataHub.io          Aleksandra Rubaj   backup Anuar Ustayev
--
-- DataHub.io is part of the existing `datopian-products` project rather than a
-- project of its own, so this rewires that one instead of creating a fourth.
--
-- CDT is NOT created here. wg-8yv.37 requires a classification per project and
-- that one has not been given; a project's visibility drives row-level security
-- and artefact inheritance, so creating it at a guessed level and correcting it
-- later moves data between visibility levels rather than editing a field. NGED
-- is `restricted` and is the comparable engagement.
--
-- Emails double as Cloudflare Access identities: the IdP is Google Workspace
-- with apps_domain datopian.com, so the address IS the identity and there is no
-- second thing to record.
BEGIN;

-- FIRST, the uniqueness that was missing.
--
-- users.primary_email had no unique constraint and no unique index. That is not
-- a tidiness gap: internal/domain/store.go binds a Cloudflare Access identity to
-- a user with
--
--     SELECT id FROM users WHERE lower(primary_email) = lower($1)
--       AND status = 'active' FOR UPDATE
--
-- read with QueryRow, which takes the FIRST row and ignores the rest. So two
-- rows differing only in case -- Anuar.Ustayev@ and anuar.ustayev@ -- were both
-- legal, and a first login would have bound the identity to whichever row
-- Postgres happened to return. A different row means different project
-- memberships, which makes it an authorisation question rather than a data one.
--
-- Indexed on lower(primary_email) to match how that query asks. A plain unique
-- constraint on the column would still admit the two rows above.
--
-- If this fails, the database already holds such a pair and it must be resolved
-- by hand: the failure is the point, because silently keeping one row would be
-- choosing somebody's identity for them.
CREATE UNIQUE INDEX IF NOT EXISTS users_primary_email_lower_key
    ON users (lower(primary_email));

-- The people. ON CONFLICT so re-applying is a no-op rather than a duplicate-key
-- failure that blocks every later migration.
INSERT INTO users (organisation_id, display_name, primary_email, status)
SELECT o.id, v.display_name, v.primary_email, 'active'
  FROM organisations o,
       (VALUES
         ('Monika Popova',    'monika.popova@datopian.com'),
         ('Joao Demenech',    'joao.demenech@datopian.com'),
         ('Aleksandra Rubaj', 'aleksandra.rubaj@datopian.com')
       ) AS v(display_name, primary_email)
 WHERE o.slug = 'datopian'
ON CONFLICT (lower(primary_email)) DO NOTHING;

-- NGED: Demenech accountable, Osahon backup.
--
-- Both owners are set in one statement. Setting them separately would leave a
-- moment where primary and backup are the same person, which the constraint
-- below refuses -- so a two-statement version fails on the first.
UPDATE projects p
   SET primary_owner_id = (SELECT id FROM users WHERE primary_email = 'joao.demenech@datopian.com'),
       backup_owner_id  = (SELECT id FROM users WHERE primary_email = 'osahon.okungbowa@datopian.com'),
       updated_at = now()
 WHERE p.slug = 'nged';

-- DataHub.io, which lives inside datopian-products: Ola accountable, Anuar backup.
UPDATE projects p
   SET primary_owner_id = (SELECT id FROM users WHERE primary_email = 'aleksandra.rubaj@datopian.com'),
       backup_owner_id  = (SELECT id FROM users WHERE primary_email = 'anuar.ustayev@datopian.com'),
       updated_at = now()
 WHERE p.slug = 'datopian-products';

-- Memberships follow the owners rather than duplicating the decision.
--
-- projects.primary_owner_id is the accountability record; project_memberships is
-- what authorisation reads. Both exist and they must agree: a project whose
-- owner cannot see it is the failure this keeps out.
DELETE FROM project_memberships m
 USING projects p
 WHERE m.project_id = p.id
   AND p.slug IN ('nged', 'datopian-products')
   AND m.role_name IN ('project_lead', 'backup_operator');

INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.primary_owner_id, 'project_lead'
  FROM projects p WHERE p.slug IN ('nged', 'datopian-products')
UNION ALL
SELECT p.id, p.backup_owner_id, 'backup_operator'
  FROM projects p WHERE p.slug IN ('nged', 'datopian-products')
ON CONFLICT DO NOTHING;

COMMIT;
