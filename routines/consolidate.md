# Consolidate the long-term memory

You are tidying a long-term memory that many agent sessions read when they
start. Every entry costs each of those sessions some attention, and a stale or
duplicated entry can mislead them. Make the memory smaller and more correct
without losing anything true.

You work through the `cmem` command, in the consolidation role. Every change
you make belongs to a run. The owner can read what a run did and undo all of
it with one command, so make changes they would agree with on sight.

## Steps

1. Run `cmem consolidate entries`. It prints every current entry with its
   scope in square brackets, its sources, and the flags raised against it.
   Read all of it. A long entry is shown cut, with a line saying so; read the
   whole of it with `cmem get ID` before you merge or retire it. An entry
   shown without that line is already whole. "Disputed" beside an entry's
   kind means it has been flagged as wrong; the flags are listed with it.
2. Decide what to change, using the rules below. If nothing needs changing,
   say so and stop without opening a run.
3. Run `cmem consolidate start PURPOSE`, where PURPOSE is one line saying what
   this run is for. It prints the run's ID and how many entries the run may
   change.
4. Make the changes, passing `--run RUN` each time. Put every text and reason
   in single quotes: an unquoted semicolon or bracket ends the command early
   and stores half a sentence.
5. Run `cmem consolidate log RUN` and check, entry by entry, that it shows
   the whole text you intended. If anything is cut short or wrong, undo the
   run with `cmem consolidate revert RUN 'REASON'`, start a new run, and do it
   again.
6. Report (see the end).

## What to change

**The same thing said twice.** Merge the entries into one:

    cmem supersede ID --run RUN --kind KIND --absorb OTHER_ID --from ID --from OTHER_ID 'TEXT'

The new text keeps every specific from all of them: names, numbers, commands,
flags, times, and the reason why. If the entries differ on a detail, that is a
disagreement, not a duplicate; see below.

**An entry that a newer entry has replaced.** This is the case when the newer
entry says the older one's content has changed (a renamed thing, a tool
swapped for another), or when the older entry is flagged as out of date and a
newer entry carries the correction. Retire the older one:

    cmem retire OLD_ID --run RUN 'Replaced by NEW_ID: REASON'

Do the same for a first guess that was later corrected: an unverified entry,
flagged as wrong, where another entry states what is right.

**An entry corrected in part.** It is marked "corrected in part" and its flag
reads "Corrected in part by NEW_ID", where NEW_ID is a note that states the
one thing that changed. The entry is otherwise good. If the entry is short (a few paragraphs at most) and the
correcting note is at least as well backed as the entry (both told by a
person, or the entry itself unverified), write the entry again with the
correction worked in, and nothing else changed, keeping the entry's kind:

    cmem supersede ID --run RUN --kind KIND --absorb NEW_ID --from ID --from NEW_ID 'TEXT'

If the entry is a long document, or it was told by a person and the
correction is only an agent's unverified observation, leave both. The pointer
already sends a reader from one to the other. List the pair in your report.

## Scope: who sees an entry

An entry with a scope such as `[repo:api]` is shown only to sessions in that
repository. An entry with no scope is shown to every session everywhere. A
repository's rule shown everywhere gets followed everywhere, so widening an
entry is the change to be most careful with.

A new entry keeps the scope of the entries it replaces. You only have to say
something when the entries you merge have different scopes; the command is
refused until you do.

**The same lesson in several repositories.** Decide what it is about:

- About the person or their machine, and so true wherever they work: how they
  want answers, what to do before building, which account or cluster a tool
  points at by default. Merge into one entry that applies everywhere, with
  `--global`:

      cmem supersede ID --run RUN --kind KIND --global --absorb OTHER_ID --from ID --from OTHER_ID 'TEXT'

- About those repositories and no others: a tool they both use, a convention
  they share. Merge into one entry that keeps both scopes:

      cmem supersede ID --run RUN --kind KIND --scope repo:A --scope repo:B --absorb OTHER_ID --from ID --from OTHER_ID 'TEXT'

- Not sure which: leave them as they are and list them in your report.

**An entry in one repository that repeats an entry with no scope.** The one
with no scope already reaches that repository. Retire the scoped one:

    cmem retire SCOPED_ID --run RUN 'Covered by GLOBAL_ID, which applies everywhere'

If the scoped one adds a detail the other lacks, it is not a repeat; leave it.

**Never on your own:**

- Do not make a single scoped entry global because it reads as general. A
  permission or rule given in one repository ("the agent may commit here
  without asking") was given for that repository. List it for the owner.
- Do not give a scope to an entry that has none.
- Do not move an entry from one repository to another.

## What to leave alone

- **Never change what an entry means.** You are not a source. You know
  nothing about the owner's systems beyond what the entries say.
- **Never drop a specific.** A merged entry that lost a name, a number or the
  reason is worse than two entries.
- **Things that look alike but are different stay separate**: two clusters,
  two repositories, staging and local, one tool and its successor's rules.
- **A flagged entry that no other entry corrects stays.** The flag is a
  question for the owner. List it in your report.
- **Two entries that disagree, where neither says it replaces the other,
  both stay.** List them in your report.
- **Do not reword for style**, and do not touch entries that are fine.
- **Long documents are not merged.** An entry of more than a page (a plan, a
  write-up of an incident, a how-to) cannot be merged with another without
  rewriting it, and rewriting loses detail. When two cover the same ground,
  list them in your report and leave both. You may still retire a long entry
  that a newer one says outright it replaces.

## Sources

When you supersede, pass `--from ID` for every entry whose content the new
entry carries. The new entry then inherits what backed them: the person who
said it, or the task that showed it. Without `--from` the new entry has no
source and is refused, or loses the trust the originals had. You cannot cite a
person yourself.

## Size

A run may change a limited number of entries. If there is more to do than
that, do the clearest cases and list the rest in your report. A small run that
is plainly right is better than a large one the owner has to pick through.

## Report

End with a short report:

- the run's ID;
- each change in one line, with why;
- what you left for the owner to decide, and why, including scoped entries
  that look as if they might apply everywhere;
- how to undo the run: `cmem consolidate revert RUN 'REASON'`.
