# 0005. Build the memory service rather than adopt one

Status: Accepted (2026-11-17)

## Context

A search on 2026-11-17 found two close existing systems:

- **Hindsight** (Vectorize, MIT): self-hosted on Postgres, exposes retain,
  recall and reflect over MCP. Its retain and reflect operations call a model.
- **Graphiti** (Zep, Apache-2.0): episodes in, time-valid facts out, history
  kept when facts change. Needs a graph database and calls a model on every
  write.

Neither was read in depth. No system found records a verifier outcome on each
record, separates consolidation permissions from worker permissions, or caps
and reverts consolidation.

## Decision

Build a small memory service of our own.

## Consequences

- We own the schema and can enforce the rules in REQUIREMENTS.md (M1 to M6)
  directly.
- The scope is small: two kinds of record, search, and an MCP endpoint.
- Hindsight remains the fallback. If building stalls, try it for an afternoon
  before investing further.
