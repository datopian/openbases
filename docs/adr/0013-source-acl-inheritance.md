# ADR-0013: Source ACL inheritance and protected sanitisation

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §14.3, §17.1, §17.2

## Context

The dangerous leak is not a copied document. It is a derived artefact: a summary that quotes a
restricted client meeting, an embedding that surfaces a confidential title in a search result, a
narrative that repeats a client's unannounced result to a marketing user. Each derivation is a
chance for classification to be silently dropped.

## Decision

Every derived artefact inherits the **most restrictive** classification of its sources,
transitively. This applies to candidates, records, narratives, context packs, embeddings, search
results, and marketing packets alike.

Publishing a sanitised derivative at a broader visibility is a protected, audited action requiring
the `knowledge.classification.downgrade` permission. A reviewer cannot widen visibility by editing
a candidate; the review interface refuses the edit unless the reviewer holds that permission.

Permission filters run **before** full-text and vector retrieval returns results, never as a
post-filter on a ranked list.

## Consequences

- Inheritance is enforced in code: `domain.InheritVisibility` returns the strictest input and
  refuses to produce a classification at all when there are no sources, so an unprovenanced artefact
  cannot receive a permissive default.
- Over-restriction will happen and will be annoying. That is the correct direction to fail.
- Isolation tests are mandatory: WP-H5 acceptance requires proving a restricted source cannot leak
  through metadata, keyword search, vector search, or a summary.
- Sanitised publication needs a real workflow, or people will route around it.

## Alternatives considered

**Classify derived artefacts independently.** Rejected: it means a model's judgement decides
whether client-confidential material is confidential.

**Post-filter search results.** Rejected: ranking, snippets, and result counts leak information
about documents the caller may not know exist.
