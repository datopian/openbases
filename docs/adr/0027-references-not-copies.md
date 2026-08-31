# ADR-0027: References and evidence spans, not copies

- **Status:** accepted
- **Date:** 2026-08-30
- **Bead:** wg-8yv.20
- **Plan reference:** §14.1, §14.3, §17.1
- **Relates to:** [ADR-0011](0011-knowledge-layers.md), [ADR-0012](0012-workspace-for-capture-git-for-durable-state.md), [ADR-0013](0013-source-acl-inheritance.md)

## Context

The question, asked plainly: a Meet transcript and a Drive file already live in
Google permanently. Do we copy them, or work from an inferred version?

ADR-0012 rejects mirroring Drive into Git as "a permission leak with extra
steps", which settles the extreme case and not the actual one. Ingestion still
has to read a transcript to get anything out of it, and something has to be kept
or there is no knowledge layer — so "do not copy" needs to say what *is* kept.

The reason this is not merely a storage question is one line in WP-H1's
acceptance criteria:

> A removed user or provider permission prevents new retrieval.

A copy cannot satisfy that. Revoke someone's access in Drive and our copy still
answers. Whatever we hold has to lose access when the source does, or the
criterion is decoration.

## Decision

**Google remains the system of record. We hold references, our own derived
statements, and the minimum evidence to justify them.**

Three tiers, and the boundaries between them are the decision.

### Transient: fetched to work, never persisted

A transcript or file is fetched, read, and discarded within the process that
needed it. Extraction, classification and summarisation all operate on it in
memory.

Nothing about this is stored. If the same document is needed again it is fetched
again — which costs an API call and buys the property that matters: every read
is authorised at the moment of reading, against Google's current ACLs, not
against a snapshot of them.

### Derived: our statements, classified and reviewed

Candidates and records are ours. They are not copies of anything; they are
assertions *about* a source, with provenance pointing at it. These persist,
carry the inherited classification (ADR-0013), and are reviewed by a person
before they become records (ADR-0011).

A summary that happens to reproduce most of a short document is a copy wearing a
derived artefact's clothes. Reviewers can see the source; the review interface
shows both.

### Evidence spans: the smallest quote that justifies a claim

A record asserting "the client agreed to X on the 14th" is unauditable if the
only support is a link to a document that has since changed. So the span that
supports a claim is kept — quoted, short, attributed to a location in the
source.

**This is a persistent partial copy, and pretending otherwise would be the
dishonest part of this decision.** It is bounded deliberately:

- a span is a quotation, not a section, and never a whole document;
- it carries the source's classification, so it is filtered by the same
  permission checks as everything else;
- it exists to make a specific claim checkable, so a span with no record
  pointing at it is deleted.

The trade is explicit: a small amount of duplicated text, classified and
attributable, in exchange for claims that can be audited after the source moves.
A system whose assertions cannot be checked is worse than one that stores a
sentence too many.

### What is not kept

Full documents. Whole transcripts. File bytes. Attachments. Nothing that would
make Workgraph a place to read Datopian's documents rather than a place to find
out what they said.

The test is uncomfortable and useful: **if the tenant lost Drive tomorrow, our
database should not be a usable replacement for it.** If it would be, we have
built a mirror and inherited every permission problem Google was solving for us.

## Consequences

**Revocation works by construction.** Access is checked at read time against the
live source, so a permission change takes effect on the next read rather than
requiring us to notice and delete something. This is the mechanism behind WP-H1's
acceptance criterion rather than a feature added to satisfy it.

**Embeddings are the exception that needs handling.** A vector is derived, and it
is also enough to surface a document's existence and rough content to someone who
should not see it. ADR-0013 already requires permission filters to run before
retrieval rather than as a post-filter; this adds that an embedding of a source
we can no longer read must be removable, so revocation has to reach the index and
not only the read path. WP-H5's isolation tests are where that gets proven.

**Deleted sources leave records behind, and that is intended.** A decision made
in a document that has since been deleted was still made. The record persists
with its provenance marked unreachable, rather than vanishing because the
document did — a knowledge layer that forgets what it learned when a file is
tidied away is not one anybody can rely on.

**Ingestion costs API calls rather than storage.** Quota becomes a constraint
where disk would have been. That is the cheaper failure: a rate limit is visible
and recoverable, while a stale mirror is invisible and wrong.

**Absence of content is not a failure.** The pilot Meet runs Monday to Thursday
and is sometimes skipped, so no transcript is produced. A monitor that alerted on
"no transcript this week" would cry wolf until it was ignored. Subscription
health and content arrival are therefore separate signals: the subscription being
active, renewed and delivering is monitored; a week with no meeting is silence,
not an incident. What *is* an incident is a conference record existing with no
transcript retrieved, because that is the shape of ingestion breaking.

## Alternatives considered

**Copy everything on ingest.** Rejected: it defeats the revocation criterion, and
it makes classification a thing we maintain rather than a thing we inherit. Every
copy is another place a classification can be dropped.

**Copy nothing at all, not even spans.** Tempting and cleaner, and rejected
because it makes every record unfalsifiable. "This was agreed" with a link to a
document that now says something else is not evidence, and a reviewer cannot
check what they cannot see.

**Cache fetched documents with a short TTL.** Rejected as the worst of both: a
cache is a copy whose staleness and permission drift are bounded by a timer
rather than by anything meaningful, and it would sit exactly where the revocation
criterion needs to bite.

**Store a content hash instead of a span.** It proves a document changed, which
is useful, and it cannot show a reviewer *what* was said. Worth adding alongside
spans; not a replacement for them.
