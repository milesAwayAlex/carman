# 0014. A note carries its evidence; humans are cited only from the laptop

Status: Proposed (2026-11-18)

## Context

M3 requires every entry to cite an episode or a named human. An agent that
learns something in the middle of a task has neither: the task's episode is
written only when the task ends, and episodes cannot be changed afterwards.

A caller could also claim anything about itself in a request: that it is the
laptop, or that a person told it something.

## Decision

- A note must say what the agent observed. The service stores that evidence as
  an episode with outcome unverified, and the entry cites it. The note is
  therefore unverified until the consolidation role replaces it with an entry
  backed by a verified episode.
- The origin of everything a caller writes (laptop or worker) is taken from its
  credential. A request cannot set it.
- Only the laptop role may cite a named human, because only there is a person
  present. An entry a human vouches for is active.

## Consequences

- Every claim in memory can be traced to what prompted it.
- Workers cannot create active entries on their own; only a passed episode or
  the consolidation role can make one.
- A human's name in a source is whatever the laptop session supplied. It is not
  authenticated beyond the laptop's token.
- Notes add small evidence episodes to the episode log.
