-- Google Workspace event sources, subscriptions and receipts (WP-H1, wg-8yv.20).
--
-- Three tables because they answer three different questions and fail
-- independently: what we are allowed to ingest, what Google currently believes
-- we are subscribed to, and what actually arrived.
BEGIN;

-- The allow-list. A source not here is ignored, and that is the acceptance
-- criterion "a non-allow-listed source is ignored" — enforced by there being
-- nothing to join to rather than by a check someone might forget.
CREATE TABLE event_sources (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- 'drive' | 'meet'. Not an enum type: adding a third means a migration
    -- either way, and a CHECK reads where it is used.
    kind          text NOT NULL CHECK (kind IN ('drive', 'meet')),

    -- The Google identifier. A shared drive id, or a Meet space name. Stable
    -- across a rename, which the display name is not — the tenant has three
    -- shared drives whose names begin "BizDev".
    external_id   text NOT NULL,

    -- What people call it. For humans reading a list; never used to match.
    display_name  text NOT NULL,

    -- The visibility every artefact derived from this source inherits
    -- (ADR-0013). NOT nullable and NOT defaulted: a source whose classification
    -- nobody chose is exactly how client-confidential material ends up in a
    -- summary the whole company can read, and inheriting the strictest source
    -- only works if the root is right.
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),

    -- Why it has that visibility, in words. A classification with no recorded
    -- reason cannot be reviewed later, only guessed at.
    rationale     text NOT NULL CHECK (length(trim(rationale)) > 0),

    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    UNIQUE (kind, external_id)
);

COMMENT ON TABLE event_sources IS
  'Allow-listed Google Workspace sources. A source absent here is ignored (WP-H1).';

-- What Google believes. One row per source, holding the subscription we created
-- and when it expires.
--
-- Separate from event_sources because they drift: a subscription expires,
-- is suspended by Google, or is deleted out from under us, and none of that
-- changes whether we are allowed to ingest the source. Reconciliation is the
-- act of comparing these two, so they cannot be one table.
CREATE TABLE event_subscriptions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id      uuid NOT NULL REFERENCES event_sources(id) ON DELETE CASCADE,

    -- Google's subscription resource name, e.g. subscriptions/abc123. NULL
    -- while we intend a subscription that does not exist yet, which is the
    -- state reconciliation acts on.
    google_name    text UNIQUE,

    -- Google's own lifecycle state, stored as it reports it rather than
    -- normalised. A state we do not recognise must not be silently mapped onto
    -- one we do.
    state          text NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending', 'active', 'suspended', 'deleted', 'failed')),

    -- Why Google suspended it, when it did. Their word, not ours.
    suspension_reason text,

    -- Subscriptions expire. This is the whole reason this table exists: an
    -- expired subscription stops delivering silently, and the only way to
    -- notice is to have written down when it would.
    expires_at     timestamptz,

    -- The event types we asked for. Recorded because the set that a
    -- shared-drive target accepts is not the documented list, and a
    -- subscription created with a different set than we now intend needs
    -- replacing rather than renewing.
    event_types    text[] NOT NULL DEFAULT '{}',

    last_renewed_at timestamptz,
    last_error     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (source_id)
);

CREATE INDEX event_subscriptions_expiring
    ON event_subscriptions (expires_at)
    WHERE state = 'active';

COMMENT ON TABLE event_subscriptions IS
  'What Google believes we are subscribed to. Compared against event_sources to reconcile (WP-H1).';

-- What arrived. An immutable receipt per delivery, written before anything acts
-- on it (ADR-0026).
CREATE TABLE event_receipts (
    -- Pub/Sub's message id IS the idempotency key. Pub/Sub is at-least-once by
    -- design, so a repeat is expected rather than exceptional, and the primary
    -- key is what makes a repeat cheap to detect.
    message_id    text PRIMARY KEY,

    -- Nullable: a delivery can arrive for a source we do not recognise, and
    -- that is worth recording rather than dropping. Recognising it later does
    -- not retroactively make it processable, but it makes it explicable.
    source_id     uuid REFERENCES event_sources(id) ON DELETE SET NULL,

    -- Google's event type, and the resource it names. The BODY is not evidence:
    -- a verified token proves the delivery came from Pub/Sub, not that its
    -- contents are true, so this records what was claimed and the work re-reads
    -- the source itself.
    event_type    text,
    target        text,

    -- The raw message, kept whole. A field we did not think to extract is the
    -- one the next investigation needs.
    payload       jsonb NOT NULL,

    received_at   timestamptz NOT NULL DEFAULT now(),
    processed_at  timestamptz,
    -- Set when processing failed. A receipt with an error and no processed_at
    -- is the queue of things to look at.
    error         text
);

CREATE INDEX event_receipts_unprocessed
    ON event_receipts (received_at)
    WHERE processed_at IS NULL;

CREATE INDEX event_receipts_source ON event_receipts (source_id, received_at DESC);

COMMENT ON TABLE event_receipts IS
  'Immutable record of every Pub/Sub delivery, written before it is acted on (ADR-0026).';

-- RLS, for the same reason every other table has it: these rows name files and
-- actors in a tenant, and the connector is not the only thing with a database
-- connection.
ALTER TABLE event_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_sources FORCE ROW LEVEL SECURITY;
ALTER TABLE event_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_subscriptions FORCE ROW LEVEL SECURITY;
ALTER TABLE event_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_receipts FORCE ROW LEVEL SECURITY;

COMMIT;
