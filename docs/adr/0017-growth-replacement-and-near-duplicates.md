# 0017. As memory grows: a larger index, replacement by working agents, and a question before a near-duplicate

Status: Accepted (2026-11-18). The owner applied the wording below to
REQUIREMENTS.md. Parts 1 to 3 are built; 4 is built under ADR 0018; 5 is planned.

## Context

Three things were seen on the transfer benchmark (`bench/transfer/RESULTS.md`)
and in reading the code against Claude Code's built-in memory:

- **Our index ran out ten times sooner than built-in memory's.** It was cut at
  2,000 characters, about 22 short lessons. Built-in memory loads the first
  200 lines or 25 KB of its index into every session. An entry outside our
  index is reachable only through flashback and recall, which are our two
  weakest parts: flashback finds 70% on the retrieval set, and every failure
  of the by-hand arm was a recall miss.
- **A changed lesson left two current entries.** When the user corrected a
  lesson, agents flagged the old entry and noted the new one, in all four
  runs. The probes passed, but the old entry stays current for good. Built-in
  memory rewrote its one file in place.
- **Agents had no way to replace an entry.** Supersession existed only for the
  consolidation role.

## Decision

1. **The index is as large as built-in memory's.** The default budget is
   24,000 characters. A list shows at most 240 characters of an entry, and
   that is what an entry costs the budget; the rest is one `cmem get` away.
   This changes the number in ADR 0016, not its design. The hook reads
   `CMEM_INDEX_BUDGET` so a harness can set its own.

2. **A working agent can replace an entry with a note.**
   `cmem note --replaces ID ...` stores the note as a new entry that
   supersedes the old one. Nothing is edited: the old note stays in the
   library and in recall "as of" an earlier time (M2).

   The rule is that the replacement must be at least as well backed as what it
   replaces. A note that rests only on the agent's own observation cannot
   replace an entry that a person or a verified task backs; the agent flags
   that entry instead. What a person says on the laptop can replace anything.

3. **A note close to an existing entry is held until the agent says which it
   is.** The service answers with the close entries and stores nothing. The
   agent sends the note again with `--replaces ID` or `--separate`. Closeness
   is word overlap, with no model: the two share at least three word stems
   covering 60% of the shorter, or five covering 40%.

4. **Planned: consolidation** (the rest of M4, then the routine itself). It is
   the only thing that makes the library smaller. Replacement by working
   agents does part of its job at the moment a lesson changes.

5. **Planned: a brief by a cheap model, as the stopgap past the index.** When
   memory holds more than the index shows, the harness has a small model read
   the whole list and name the entries that matter for the task; `cmem` then
   prints those entries as written. The service still calls no model
   (ADR 0006). It is to be measured on the transfer benchmark against a memory
   of a few hundred lessons before it is relied on.

## A sentence in REQUIREMENTS.md this departs from

"Proof of concept" lists "supersede, retire and revert behind a separate
credential". With this decision a working agent's credential can also
supersede, within the rule in point 2. The "Done when" checks of M2 and M4
still hold: no operation edits or deletes, and the consolidation role is still
limited to add, supersede and retire with its own credential.

Proposed wording, for the owner to apply if they agree:

> In scope: episodes and entries; recall, get, note and flag; replacing an
> entry with a note that is at least as well backed; the end-of-task record
> endpoint; unrestricted supersede, retire and revert behind a separate
> credential; ...

## Consequences

- A session may start with up to about 6,000 tokens of memory, as it may with
  built-in memory. Nothing has been measured yet with a memory that large; the
  22-lesson runs showed no cost and no misapplied lesson.
- The closeness rule was set on a small sample: the lessons agents wrote in
  the benchmark, and the retrieval set, where no two lessons count as close.
  A wrong "close" costs the agent one more command. A missed one leaves two
  current entries, as before.
- Holding a note risks the agent not sending it again, and a lesson not
  written is the worst outcome seen so far. The benchmark's "wrote the lesson"
  column is the check.
- A worker can never replace what a person said, however good its evidence.
  That waits for consolidation, which can cite verified episodes.
- Replacement by an agent is not yet reversible as a unit. Revert arrives with
  the rest of M4.
