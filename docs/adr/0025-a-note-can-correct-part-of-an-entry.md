# 0025: A note can correct part of an entry

Status: Proposed

## Context

Some entries are long: a working document of tens of thousands of characters
that an agent kept over weeks. An agent that finds one fact in such an entry
out of date has two tools, and neither fits. `--replaces` puts its note in
place of the whole entry, which would throw away everything else the entry
says. `cmem flag` says the entry is wrong without saying what is right.

An unattended agent given this situation did the sensible thing by hand: it
flagged the entry, then stored the right fact as a separate note. Nothing
tied the two together. The next reader of the long entry saw "flagged as
wrong" and had to search for the correction.

## Decision

`cmem note --amends ID ...` stores the note beside the entry it corrects and
flags that entry with a reason that names the new note and quotes its opening:
"Corrected in part by NEW-ID: ...". The amended entry is kept whole. Wherever
it is shown, in `get` and in a search result, the pointer is shown with it.

This adds no kind of record. It is the two writes the agent made by hand,
made together, with the pointer the agent left out. Any role that may flag
may amend, and the note itself is held to the same rules as any other note.

The acknowledgement of a plain `cmem flag` now suggests `--amends` and
`--replaces`, since a flag alone leaves the next reader without the answer.

## Consequences

- The amended entry counts as disputed: it is marked so in lists and sorts
  after undisputed entries of its kind. That is accurate, but coarse. An
  entry that is right in ninety-nine places and corrected in one is treated
  like one that is wrong throughout.
- Merging a correction into the entry it corrects is left to consolidation,
  which can supersede the long entry with a corrected text. The routine does
  not do this yet for very long entries.
- If the pointer cannot be written after the note has been stored, the
  command says so and names the stored note; the note is not taken back.

## Alternatives considered

- **A header on the correcting note (`amends: ID`), followed when the entry
  is shown.** Cleaner to read back and does not mark the entry as disputed,
  but it is a change to the note format and to every place an entry is
  shown. Worth doing if partial corrections turn out to be common.
- **Letting an agent replace a long entry with an edited copy.** The agent
  would have to read and reproduce the whole text to change one line, and a
  slip would lose the rest silently.
