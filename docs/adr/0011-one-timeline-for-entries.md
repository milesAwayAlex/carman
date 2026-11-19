# 0011. Entries have one timeline: when the memory held them as current

Status: Accepted (2026-11-19, as proven in use; proposed 2026-11-18). The owner is unsure; the benchmark's
changed-fact tasks are meant to settle it.

## Context

M2 requires that a recall "as of" a past date returns what was current then.
There are two things "current" could mean: when a fact was true in the world,
or when the memory believed it. Temporal knowledge graphs such as Graphiti
track both. Tracking when a fact was true requires someone to assert that date,
and agents will often not know it. Claude Code's built-in memory, the bar for
M7, tracks neither.

## Decision

Track one timeline. An entry is current from when its note was written until a
later entry supersedes it. Both moments are stamped by the service's clock,
never by the caller. A recall as of time T returns the entries the memory held
as current at T.

## Consequences

- "As of" answers "what did the memory believe then", which is what an audit of
  an agent's past behaviour needs.
- It cannot answer "what was true in the world on that date" when the memory
  learned of a change late. If the benchmark shows that matters, a note can
  carry a second, asserted date without changing existing notes.
- Because the caller cannot set the times, history cannot be backdated.
