# 0008. Rows with supersession, not a graph database

Status: Superseded by 0015 (2026-11-18), before it was accepted. Supersession without a graph database survives there, as write-once notes.

## Context

Temporal knowledge graphs resist memory erosion because a new fact supersedes
an old one and the old one stays on record. The ACE paper found the same
property in itemised entries with incremental updates, and found that memory
rewritten wholesale collapses into generic summaries.

A graph database adds traversal across several hops. It also requires entities
to be extracted and matched at write time, and those mistakes are hard to see.

## Decision

Store memory as rows in Postgres. Entries are never edited in place; a change
creates a new entry that supersedes the old one.

## Consequences

- The `supersedes` and `sources` links already form a small graph.
- If questions that need several hops turn out to matter, typed relations
  between entries can be added without changing the agent-facing tools.
