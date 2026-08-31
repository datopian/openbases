-- Register the pilot's allow-listed sources (wg-8yv.38).
--
-- Seeded in a migration rather than by hand, so the allow-list a deployment
-- runs with is the one in the repository. A source added directly to the
-- database is one nobody reviewed, and this table decides what an automated
-- system may read across the whole tenant.
--
-- Every row states its visibility and WHY. ADR-0013 inherits the strictest
-- classification of an artefact's sources transitively, which only works if the
-- root is right — so there is no default and no nullable column to forget.
BEGIN;

INSERT INTO event_sources (kind, external_id, display_name, visibility, rationale)
VALUES
    -- The three shared drives every employee can already read. A drive whose
    -- membership is the whole company cannot leak to the whole company, which
    -- is why these are the ones to start with: the isolation tests can be built
    -- before the drives that need them.
    ('drive', '0ACuIgKcIt7SPUk9PVA', 'All',
     'internal',
     'Shared drive readable by everyone in the company; membership is the whole company'),
    ('drive', '0AEPg8vnj02IcUk9PVA', 'BizDev',
     'internal',
     'Shared drive readable by everyone in the company; membership is the whole company'),
    ('drive', '0ADdEMMAO5SMVUk9PVA', 'Delivery',
     'internal',
     'Shared drive readable by everyone in the company; membership is the whole company'),

    -- The registered recurring Meet. Monday to Thursday, sometimes skipped, so
    -- absence of a transcript is silence rather than an incident (ADR-0027).
    -- The stable space id, not the typeable meeting code. tfy-qcsa-twb is an
    -- alias that resolves to this, and an alias is the same trap as matching a
    -- shared drive by name.
    ('meet', 'FS4Sj-9MIY0B', 'Innovation Team Sync',
     'internal',
     'Internal product sync for DataHub.io; attendees are Datopian staff')
ON CONFLICT (kind, external_id) DO NOTHING;

COMMIT;
