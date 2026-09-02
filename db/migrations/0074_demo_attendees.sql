-- wg:backfill — the eight people attending the 3 September session (B3).
--
-- Attendees, decided by Anuar 2026-09-02:
--
--   anuar.ustayev      daniela.popova    osahon.okungbowa   rufus.pollock
--   joao.demenech      monika.popova     aleksandra.rubaj   luccas.mateus
--
-- The first four already hold company-wide organisation_admin or executive
-- grants (0009, 0066) and reach every project through those. The other four
-- reach only what their memberships say, which is what this adds -- and 0071
-- makes that the difference between seeing the demo project and seeing an empty
-- page, because the work overview now filters by membership.
--
-- Emails double as Cloudflare Access identities: the IdP is Google Workspace
-- with apps_domain datopian.com, so the address IS the identity and there is no
-- second thing to record. The matching allow-list change is in
-- infra/tofu/envs/staging/terraform.tfvars; a users row without the allow-list
-- entry cannot log in, and an allow-list entry without a users row logs in and
-- resolves to nobody.
BEGIN;

-- Luccas is the only attendee with no users row. The other seven exist from
-- 0009 and 0049.
INSERT INTO users (organisation_id, display_name, primary_email, status)
SELECT o.id, 'Luccas Mateus', 'luccas.mateus@datopian.com', 'active'
  FROM organisations o
 WHERE o.slug = 'datopian'
ON CONFLICT DO NOTHING;

-- portaljs-oss and roseville-poc for every attendee: the two internal projects
-- the demo touches.
--
-- 'contributor' rather than a lead role. A membership is what makes the work
-- visible and the project selectable; it is not a claim about who runs the
-- engagement, and the owner columns already say that.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, u.id, 'contributor'
  FROM projects p
  JOIN users u ON u.primary_email IN (
        'anuar.ustayev@datopian.com',
        'daniela.popova@datopian.com',
        'osahon.okungbowa@datopian.com',
        'rufus.pollock@datopian.com',
        'joao.demenech@datopian.com',
        'monika.popova@datopian.com',
        'aleksandra.rubaj@datopian.com',
        'luccas.mateus@datopian.com')
 WHERE p.slug IN ('portaljs-oss', 'roseville-poc')
ON CONFLICT DO NOTHING;

-- CDT and NGED memberships are NOT granted here, and that is the point of
-- listing the projects above rather than looping over all of them.
--
-- Those two are restricted client engagements with named operators -- Monika on
-- CDT, Joao on NGED, Osahon backup on both (0049, 0050) -- and being in the
-- room for a demo is not a reason to gain access to a client's material. The
-- four company-wide role holders already read them through their grants, which
-- is a decision that was taken and recorded (mem-003b900b), not something this
-- migration extends.

COMMIT;
