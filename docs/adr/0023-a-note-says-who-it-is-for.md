# 0023. A new note says who it is for

Status: Accepted (2026-11-19). Built, and on in the Claude Code adapter's
settings.

## Context

Every note a working agent wrote had no scope, so it was shown to every
session everywhere (ADR 0021 left this open, and DESIGN.md carried it as Q8).
That is what carried lessons from one repository to another. It also meant a
lesson about one repository was applied in the others.

An experiment counted both (bench/transfer/RESULTS.md, 2026-11-19; Sonnet, one
run per cell). With every note global, 3 of 3 general lessons reached another
repository and 1 of 2 repository lessons leaked into it. With the agent made
to choose, under the wording below, 3 of 3 reached it and 0 of 2 leaked.

## Decision

- With `CMEM_NOTE_SCOPE=ask`, a new note must say who it is for: `--here`,
  which scopes it to the repository the agent is in (ADR 0022), or `--global`.
  A note that says neither is refused with a line explaining the choice. A
  replacement is not asked; it keeps the scope of what it replaces (ADR 0021).
- The Claude Code adapter's settings turn this on, and the session text
  explains the choice in one sentence: `--here` when it is true of this
  repository only, `--global` when it would also hold in another repository,
  with the user's preferences, their machine, and how their team or
  organisation does things as examples.
- The test is "would it hold in another repository?", not "is it about the
  user?". The first wording said global was for what is about the user or
  the machine; agents then kept an organisation-wide rule to one repository,
  and 2 of 3 general lessons stopped travelling.
- There is no default. Either default would be wrong often enough, silently.
- Without the setting the command behaves as before, so other harnesses and
  scripts are not broken by this.

## Consequences

- A general lesson wrongly marked `--here` does not travel until it is learned
  again in a second repository, when consolidation merges the two into one
  global entry (ADR 0021), or until someone says it applies everywhere.
- A repository lesson wrongly marked `--global` leaks as before, and nothing
  narrows it automatically.
- The wording was fitted to the six lessons of one experiment. It is one
  sentence in every session's index, and changing it changes behaviour, as
  the first version showed.
- A middle level, true across one organisation's repositories but not the
  owner's others, has no tag yet. Such lessons are marked global today.
