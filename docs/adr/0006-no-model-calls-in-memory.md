# 0006. The memory service makes no model calls

Status: Accepted (2026-11-17). The owner may revisit this later.

## Context

Memory should be independent of inference (R5). Inference is billed to the
owner's subscription (0002), so a service that called a model itself would need
a separate provider.

## Decision

The memory service stores, searches and enforces rules. It never calls a model.
Extraction, consolidation and retrieval reasoning are done by agents that call
the service.

## Consequences

- Consolidation ("dreaming") is an agent run that submits changes to the
  service, not a feature inside it.
- Search inside the service is limited to what needs no model, unless an
  embedding model is added. Whether embeddings count as an exception to this
  decision is open (DESIGN.md, open questions).
