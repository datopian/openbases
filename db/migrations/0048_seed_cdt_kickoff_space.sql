-- wg:backfill — registers one allow-listed event source.
--
-- The CDT internal kick-off Meet space (wg-8yv.37).
--
-- Registered ahead of the meeting rather than after it, because a Workspace
-- Events subscription only delivers what happens AFTER it exists. There is no
-- backfill: an unsubscribed conference raises no event, and while the transcript
-- still lands in Drive and can be found later through conferenceRecords.list,
-- "we noticed" and "we can reconstruct it if someone asks" are different
-- properties. The kick-off is today.
--
-- Space code rtd-siqf-aup resolves to spaces/44KbrlezvqkB, resolved through the
-- Meet API under delegation rather than typed from the invite. The code is what
-- humans see and the id is what the API needs, and they are not interchangeable.
BEGIN;

INSERT INTO event_sources (kind, external_id, display_name, visibility, rationale)
VALUES (
    'meet',
    '44KbrlezvqkB',
    'CDT internal kick-off',
    -- confidential, not internal, and deliberately the stricter of the two.
    --
    -- Every artefact derived from a source inherits its classification
    -- (ADR-0013), so this is the decision that determines whether a summary of
    -- this meeting can be read by everyone at Datopian. A client engagement
    -- that is pre-contract carries commercial terms and the client's own
    -- material; the cost of it being too strict is that someone has to ask for
    -- access, and the cost of it being too loose is unrecoverable.
    'confidential',
    'Client engagement, pre-contract. Commercial terms and client material, so '
    'derived artefacts must not inherit company-wide visibility. Registered for '
    'the internal kick-off on 2026-09-01; space code rtd-siqf-aup.'
)
ON CONFLICT (kind, external_id) DO NOTHING;

COMMIT;
