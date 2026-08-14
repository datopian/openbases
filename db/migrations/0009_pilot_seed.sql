-- 0009_pilot_seed.sql — the pilot organisation, users, portfolios and projects.
-- Work package: WP-C3. Plan sections 3.1, 20 (Wave 1).
--
-- wg:backfill — this migration inserts rows rather than only changing schema.
-- It is the registry bootstrap: without an organisation and at least one user,
-- nobody can authenticate, so this cannot be done through the API it enables.
--
-- Every insert is idempotent on a natural key, so re-running is safe and the
-- registry can be rebuilt from code (ADR-0015).
--
-- Identity subjects are deliberately NOT set here. A Cloudflare Access subject
-- is issued by the provider and is not knowable in advance; WP-C3's onboarding
-- links it on first sight, and until then a user exists but cannot sign in.

BEGIN;

INSERT INTO organisations (slug, name)
VALUES ('datopian', 'Datopian')
ON CONFLICT (slug) DO NOTHING;

-- Pilot people. Roles are recorded in docs/pilot/registry.md; the grants
-- themselves are role_grants rows created below.
INSERT INTO users (organisation_id, display_name, primary_email)
SELECT o.id, v.display_name, v.email
FROM organisations o,
     (VALUES
        ('Anuar Ustayev',    'anuar.ustayev@datopian.com'),
        ('Rufus Pollock',    'rufus.pollock@datopian.com'),
        ('Osahon Okungbowa', 'osahon.okungbowa@datopian.com'),
        ('Daniela Popova',   'daniela.popova@datopian.com')
     ) AS v(display_name, email)
WHERE o.slug = 'datopian'
  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.primary_email = v.email);

INSERT INTO portfolios (organisation_id, slug, name, kind)
SELECT o.id, v.slug, v.name, v.kind
FROM organisations o,
     (VALUES
        ('oss',      'Open Source',        'oss'),
        ('product',  'Products',           'product'),
        ('client',   'Client Engagements', 'client'),
        ('internal', 'Internal',           'internal')
     ) AS v(slug, name, kind)
WHERE o.slug = 'datopian'
ON CONFLICT (organisation_id, slug) DO NOTHING;

-- Organisation-scoped role grants.
INSERT INTO role_grants (user_id, role_name, organisation_id)
SELECT u.id, v.role_name, o.id
FROM organisations o
JOIN users u ON u.organisation_id = o.id
JOIN (VALUES
        ('anuar.ustayev@datopian.com', 'organisation_admin'),
        ('rufus.pollock@datopian.com', 'executive'),
        ('daniela.popova@datopian.com', 'function_lead')
     ) AS v(email, role_name) ON v.email = u.primary_email
WHERE o.slug = 'datopian'
ON CONFLICT DO NOTHING;

-- The three pilot projects. The restricted one has no execution cell recorded
-- yet: cells are registered by WP-E1, and the schema refuses a restricted
-- project without one, so nged is created as confidential and tightened to
-- restricted when its cell exists. Creating it restricted now would fail, and
-- weakening the constraint to allow it would defeat the constraint.
INSERT INTO projects (organisation_id, portfolio_id, slug, name, objective,
                      visibility, primary_owner_id, backup_owner_id)
SELECT o.id, pf.id, v.slug, v.name, v.objective, v.visibility, po.id, bo.id
FROM (VALUES
   ('portaljs-oss', 'PortalJS and open source', 'Maintain Datopian open-source projects', 'oss', 'internal',
    'rufus.pollock@datopian.com', 'osahon.okungbowa@datopian.com'),
   ('datopian-products', 'Datopian products', 'Product delivery across PortalJS Cloud and DataHub', 'product', 'internal',
    'anuar.ustayev@datopian.com', 'daniela.popova@datopian.com'),
   ('nged', 'NGED', 'Restricted client engagement', 'client', 'confidential',
    'osahon.okungbowa@datopian.com', 'rufus.pollock@datopian.com')
 ) AS v(slug, name, objective, portfolio, visibility, primary_owner, backup_owner)
JOIN organisations o  ON o.slug = 'datopian'
JOIN portfolios pf    ON pf.organisation_id = o.id AND pf.slug = v.portfolio
JOIN users po         ON po.organisation_id = o.id AND po.primary_email = v.primary_owner
JOIN users bo         ON bo.organisation_id = o.id AND bo.primary_email = v.backup_owner
WHERE NOT EXISTS (SELECT 1 FROM projects p WHERE p.organisation_id = o.id AND p.slug = v.slug);

-- Membership follows ownership: an owner who is not a member cannot read their
-- own project, because the RLS policy checks membership rather than ownership.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.primary_owner_id, 'project_lead'
FROM projects p
ON CONFLICT DO NOTHING;

INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.backup_owner_id, 'backup_operator'
FROM projects p
ON CONFLICT DO NOTHING;

INSERT INTO project_repositories (project_id, owner, name)
SELECT p.id, v.owner, v.name
FROM projects p
JOIN (VALUES
   ('portaljs-oss', 'datopian', 'portaljs'),
   ('portaljs-oss', 'datopian', 'wayintoai'),
   ('portaljs-oss', 'datopian', 'giftless'),
   ('portaljs-oss', 'datopian', 'autoclaw.sh'),
   ('portaljs-oss', 'ckan',     'ckan'),
   ('portaljs-oss', 'flowershow','flowershow'),
   ('datopian-products', 'datopian', 'postal-codes'),
   ('datopian-products', 'datopian', 'sre-agent'),
   ('datopian-products', 'datopian', 'cloud.portaljs.com'),
   ('datopian-products', 'datopian', 'datahub-next'),
   ('nged', 'datopian', 'nged')
 ) AS v(project_slug, owner, name) ON v.project_slug = p.slug
ON CONFLICT (provider, owner, name) DO NOTHING;

COMMIT;
