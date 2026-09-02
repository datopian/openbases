-- The roseville-poc project, for the 3 September demo (B2).
--
-- Internal, not restricted: Roseville is a prospect and everything in the
-- proof of concept comes from their PUBLIC Socrata catalogue. Nothing here is
-- client-confidential, and classifying it restricted would put it in a cell of
-- its own for no reason -- restricted_requires_cell exists precisely because a
-- restricted project needs one (plan section 7.4).
--
-- On the oss cell, which is where the demo dispatches. Not a client cell: the
-- global constraint for the demo is that CDT, NGED and DataHub cells are not
-- touched.
--
-- Memberships are NOT created here. The attendee list is a separate input and
-- goes in with the Access allow-list (B3), so that the people who can log in
-- and the people who can select this project are decided together rather than
-- drifting apart. Company-wide role holders already reach it through their
-- grants.
BEGIN;

INSERT INTO projects (organisation_id, slug, name, objective, visibility,
                      primary_owner_id, backup_owner_id, execution_cell_id)
SELECT o.id,
       'roseville-poc',
       'City of Roseville open data proof of concept',
       'Show a working PortalJS portal built from Roseville''s public Socrata catalogue, and the workgraph that produced it.',
       'internal',
       po.id, bo.id, c.id
  FROM organisations o
  JOIN users po ON po.primary_email = 'anuar.ustayev@datopian.com'
  JOIN users bo ON bo.primary_email = 'osahon.okungbowa@datopian.com'
  LEFT JOIN execution_cells c ON c.slug = 'oss'
 WHERE o.slug = 'datopian'
ON CONFLICT (organisation_id, slug) DO NOTHING;

COMMIT;
