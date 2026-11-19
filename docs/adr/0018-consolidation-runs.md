# 0018. Consolidation works in runs that are logged, capped and reverted by a note

Status: Accepted (2026-11-18). Built.

## Context

M4 asks that consolidation make small, reversible changes: the role can only
add, supersede and retire; each run's changes are logged, capped in size, and
can be reverted as a unit. Supersede existed. Retire, runs, the cap and revert
did not.

The library is write-once (ADR 0015), so "revert" cannot mean deleting what a
run wrote, and `git revert` would remove notes from the working tree, which
`cmem check` rightly reports as tampering.

## Decision

- **A run is a note.** `cmem consolidate start PURPOSE` writes it and returns
  its ID. Every add, supersede and retire by the consolidation role must name
  a run, and the note it writes carries the run's ID.
- **Retiring is a note** that names the entry and gives the reason. The entry
  stops being current from then on.
- **Merging is a supersede that absorbs.** The new entry supersedes one entry
  and lists the others under `absorbs`. All of them stop being current.
- **Reverting is a note** that names the run. From then on nothing the run
  wrote counts: its entries are not current, and the entries it replaced or
  retired are current again, unless something outside the run has replaced
  them since. A reverted run takes no more changes.
- **The cap** is on entries changed per run: one for an add or a retire, one
  per entry replaced in a supersede. The default is 20 (`CMEM_RUN_CAP`).
- **The log** is the run with its list of changes, readable by every role
  (`cmem consolidate log`). Each change is also its own git commit.
- **Sources are inherited, not claimed.** The consolidation role cannot cite a
  person. When it rewrites or merges entries it names them with `--from`, and
  the service copies their sources onto the new entry. A merged entry
  therefore stays trusted exactly when one of its originals was.

## Consequences

- An entry's time as current is no longer one interval: an entry replaced by a
  run and restored by its revert was current, then not, then current again.
  "As of" a past time is answered from the notes that existed and counted at
  that time, so it still returns what the memory held then (M2).
- Reverting a run does not undo what working agents did on top of it. If an
  agent replaced an entry the run had added, that replacement stays current
  and the originals come back beside it. The next run has to sort that out.
- If the role rewrites an entry and forgets `--from`, the new entry loses the
  person who backed the original and has to rest on an episode or be refused.
  The routine's prompt has to say so.
- A wrong flag cannot be dismissed directly. The role supersedes the flagged
  entry with the same text, which gives a current entry with no flags.
- Listing episodes for the role is still to build. Until workers record
  episodes there is little for it to read besides entries and flags.
