# Transfer benchmark: results so far

Raw results and every transcript are under `$CARMAN_BENCH_ROOT/transfer/<run>/`
on the machine that ran them; they are not in this repository.

Each case teaches a lesson by a correction in repository A, then probes it in a
fresh session in repository A and in repository B. "Wrote" means the lesson was
in memory after the teach session.

## 2026-11-24, consolidation that reads job definitions, runs restricted (Sonnet 5.5, one run)

`scripts/bench-transfer.sh consolidate --no-probes`, after ADR 0044. The messy
library gained three cases: an instruction for one job that its definition
already holds, one that its definition contradicts, and a note that adds to
an entry (`--adds-to`). The routine could read three definition files
(`agents.yaml`, two routines), and for the first time ran with the limits a
restricted routine gets in the cluster (`cmem routine --flags`: `dontAsk`,
Bash, Read, Glob and Grep, `Bash(cmem *)`, the definitions directory, and
the appended system prompt) instead of every tool.

- All 22 checks passed, and reverting the run restored the library.
- It found the definitions with Glob and read all three. It retired the
  covered instruction with the reason "it is an instruction for one job, and
  the definition holds it", quoting `price-review.md`; it kept the
  contradicted one and asked in one line which schedule is right, saying
  what it would do with each answer.
- It merged the entry and the note that added to it, with both specifics
  and nothing dropped ("the second only added detail").
- No call was refused. It joined many `cmem` writes into a few Bash calls,
  so the session took 8 turns; estimated $0.094.
- Probes were not run (`--no-probes`), to save cost.

## 2026-11-19, distilling finished documents, three models (one run each)

`run.py distill`. Four documents are in memory: the notes of a finished
migration and a closed incident review (2,600 to 3,500 characters, with
seven lessons planted among details of the moment), a rollout still in
progress, and a runbook in use. The routine (`routines/distill.md`) should
turn each finished document into lessons and a pointer, and leave the other
two alone. Thirteen mechanical checks.

| Model | Checks | Lessons found | Details carried over | Writes refused | Cost | Time |
|---|---|---|---|---|---|---|
| Haiku 5.5 | 11/13 | 7/7 | 0/15 | 0 | $0.013 | 44 s |
| Sonnet 5.5 | 13/13 | 7/7 | 0/15 | 0 | $0.114 | 29 s |
| Opus 5.5 | 13/13 | 7/7 | 0/15 | 0 | $0.370 | 56 s |

- **This does not tell the models apart.** All three found every planted
  lesson, carried over none of the details of the moment, left the document
  in progress and the runbook alone, and kept scope and backing. Haiku's two
  failures are one thing: its pointers said what the document was and not
  how to read it.
- Read by eye, the lessons are good from all three: one to three sentences,
  the command exactly as the document gives it. Each model also kept one or
  two sound lessons that were not planted (the role that could not change an
  alias; the alert window that was too long).
- No write was refused, so the check that a lesson says nothing its
  document does not has still not been met by a model that breaks it.
- Current text went from 11,800 characters to about 8,400. The saving is
  modest here because two of the four documents are meant to stay.
- **What this test is too easy to show.** Its documents are short and say
  their lessons almost in so many words. Real ones run to 50,000 or 200,000
  characters, their lessons are implied, and a decision made in one month
  is reversed in another. Harder documents are needed before a stronger
  model can be said to be worth its price, or not.

## 2026-11-19, correcting one fact in a long entry (Sonnet 5.5, cmem with the hook)

Two new cases (`--kind document`). A long entry is already in memory: a
release runbook of about 3,000 characters, and working notes of about 2,800.
A session is asked something the entry answers, then corrected on that one
fact ("everything else still holds"). A fresh session is asked for the
changed fact, and another for a fact the correction did not touch.

The question was whether the session-start text should offer `--amends`. Two
arms: the text as installed, and a text with one more sentence ("If it is one
thing in a long entry whose rest still holds, add `--amends ID` instead").
Three runs of each case per arm.

| Session-start text | Changed fact | Untouched fact | First move was `--replaces` with a summary | Entry kept and amended | Entry replaced by a summary |
|---|---|---|---|---|---|
| As installed | 6/6 | 5/6 | 6/6 | 4/6 | 2/6 |
| Also offering `--amends` | 6/6 | 6/6 | 6/6 | 3/6 | 3/6 |

- **The wording changed nothing.** In all twelve runs the agent read the
  entry, and on being corrected replaced it with a summary that ended
  "everything else in the earlier runbook still holds". The sentence about
  `--amends` was in front of half of them. It is not in the text now.
- **The question at the point of action worked every time it was asked.** A
  note given with `--replaces` against a long entry was held when it was
  under a quarter of the entry's length, and the agent was offered `--amends`
  or `--whole`. Seven notes were held; seven agents ran the command again
  with `--amends`, with a shorter note about the one fact.
- **The threshold was too low.** Summaries of 26% to 43% of the runbook were
  not held, and replaced it in 5 of 6 runs. One had dropped the rollback
  command, and the session asked for it answered with a command that does
  not exist. The others kept it by luck of what the summary chose to keep.
  The question is now asked of any note under three quarters of a long
  entry's length. Re-run below.
- Cost was the same in both arms: about $0.12 a case.

The two changing-lesson cases, two runs per arm, were identical in both arms:
4/4 replaced with `--replaces`, one current entry at the end, the same
repository 4/4. **The other repository was 2/4, and that is a finding of its
own.** In the kubectl-context case all four agents recorded the first lesson
with `--global`, and a later session replaced it with `--here --replaces ID`.
The replacement took the scope it was given, so the corrected rule was shown
in one repository only. This case had not been run since notes had to say who
they are for. `--here` no longer changes who a replacement is for (ADR 0026,
proposed). Re-run below.

**After both fixes, on Haiku 5.5** (the installed text; a weaker model than
the runs above, so this is not a like-for-like re-run):

| Cases | Changed fact | Untouched fact | Other repository | How the correction was stored |
|---|---|---|---|---|
| Document, 6 runs | 6/6 | 6/6 | | 2 amended after being asked; 4 a new note beside the entry |
| Changing, 4 runs | 3/4 | | 3/4 | 3 replaced; 1 stored nothing |

- No long entry was replaced. Two agents tried `--replaces`, were asked, and
  switched to `--amends`.
- Four agents did not touch the entry at all: they wrote the new fact as a
  plain note. It was not close enough to the entry's opening to be held, so
  memory ended with the stale entry and the correction side by side and
  nothing joining them. The probes still passed, because the newer note was
  found first. That is the next gap: a correction that never names what it
  corrects.
- The scope fix was not exercised on Haiku. In the kubectl-context case one
  agent replaced with `--global` of its own accord and the rule reached the
  other repository; the other agent recorded nothing in either session,
  which is the one failure.

**The document cases again on Sonnet 5.5, three runs each:** changed fact
6/6, untouched fact 6/6, and all six entries kept and amended (it was 4 of 6,
with 2 replaced by a summary). Every agent's first move was still
`--replaces` with a summary; every one was asked, and every one ran it again
with `--amends`. Cost unchanged at about $0.12 a case.

**The kubectl-context case again on Sonnet 5.5, four runs:** same repository
4/4, other repository 4/4 (it was 0/4). All four agents did what they did
before, `--global` for the first lesson and `--here --replaces ID` for the
correction, and all four entries still applied everywhere afterwards.

## 2026-11-19, who a note is for (Sonnet 5.5, cmem with the hook, one run per cell)

Until now every note an agent wrote had no scope and was shown everywhere.
This experiment counts both ways that can go wrong. Three lessons are general
and should reach another repository (the three used throughout). Three are
about one repository and should not: a build tag its tests need, its default
branch, and its development server's port. Each is taught in one checkout and
probed there, in a second checkout of the same repository under a different
directory name, and in another repository. Repositories are told apart by
their origin remote (ADR 0022).

| Notes | General lessons that reached the other repository | Repository lessons that leaked into it | Applied in the second checkout |
|---|---|---|---|
| All global (today's behaviour) | 3/3 | 1/2 | 5/5 |
| Must say `--here` or `--global`, first wording | 1/3 | 0/2 | 5/5 |
| Must say `--here` or `--global`, second wording | 3/3 | 0/2 | 5/5 |

Counts are of lessons that were written; see the last point.

- **All global leaks.** The build tag learned in one repository was used in
  the other. The port was not, only because the note happened to name its
  repository and the agent in the other one noticed.
- **The first wording stopped leaks and broke carry-over.** It said global was
  for what is "about the user or this machine". Two general lessons were
  marked `--here`; one of those notes itself said "all JavaScript projects in
  this org use pnpm". An organisation's rule is neither about the user nor
  about the machine, and the agent took the wording at its word.
- **The second wording asks whether it would also hold in another
  repository**, and names the team or organisation as an example. All three
  general lessons were marked global and both written repository lessons
  `--here`. The agent chose on its first try every time; the refusal for a
  note that does not say never fired.
- **Naming a repository by its origin worked throughout.** Every lesson scoped
  to a repository was applied in the second checkout, whose directory name
  differs, and the benchmark's containers have no git installed.
- **The default-branch lesson was never written, in any of three runs.** The
  agent corrected its answer and saved nothing. It is the first lesson cmem
  has failed to record. A default branch is something a repository says for
  itself, which may be why it was not thought worth keeping; the benchmark's
  checkouts have no git to ask, so this case cannot tell a sound judgement
  from a miss. It is left out of the counts above.

One run per cell, and the second wording was written after seeing the first
fail on these same lessons, so it is fitted to them.

## 2026-11-18, consolidation with scopes

The messy library gained entries scoped to two repositories: a preference
noted in both, a tool both use, a repository's copy of a global rule, a
duplicate within one repository, and a permission given in one repository.
38 entries, 19 checks. The routine has rules for these (ADR 0021).

| Consolidator | Entries after | Changed | Checks | Est. cost |
|---|---|---|---|---|
| Sonnet 5.5 | 30 | 12 | 19/19 | $0.08 |
| Haiku 5.5 | 30 | 12 | 19/19 | $0.01 |

- Both made the shared preference one global entry, kept the shared tool out
  of other repositories, retired the repository's copy of the global rule,
  kept the within-repository merge in its repository, and left the permission
  where it was given.
- **Haiku's first run stored four merged entries cut short**, because it left
  the texts unquoted and the shell split them at semicolons. It saw this in
  the run's log, reverted the run, and did it again correctly. That is the log
  and the undo doing their job, and also a hazard of the routine as written:
  its examples showed the text unquoted. They are quoted now, and the routine
  says to check the log entry by entry.
- One run each. The Sonnet run's revert restored memory exactly.

## 2026-11-18, a memory larger than the index (Sonnet 5.5, one run per cell)

Each arm's memory holds 264 unrelated lessons (`--crowd 264`), which is more
than either memory loads at session start. In the cmem arms they are added
after the lesson is taught, so the lesson is the oldest entry and the first to
fall out of the index. In the built-in arm they are there beforehand, so a
lesson appended to the index lands past its 200th line.

| Memory | Wrote | Same repository | Other repository | Est. cost per case |
|---|---|---|---|---|
| Built-in, one shared directory | 1/3 | 1/3 | 1/3 | $0.25 |
| cmem, index in one hook (62 of 265 shown) | 3/3 | 3/3 | 3/3 | $0.15 |
| cmem, index in five hooks (all shown) | 3/3 | 3/3 | 3/3 | $0.19 |
| cmem, one hook plus the brief by Haiku | 3/3 | 3/3 | 3/3 | $0.10 |

What the transcripts showed:

- **With one hook the lesson was never in the index, and flashback delivered
  it every time**, mostly on the agent's own reply, which costs a turn or two
  (three or four turns where the other arms took two).
- **With five hooks the lesson was in the index and was used**, at the highest
  cost of the cmem arms: every session carries all 265 entries.
- **The brief picked exactly the lesson in 6 of 6 probes** and nothing else,
  for about $0.001 a call. It was the cheapest arm, because the main session
  carries the short index and needs no extra turn.
- **Built-in memory's two failures were lessons it did not write**, the same
  failure as at small scale, on the same two lessons. On the lesson it did
  write, the agent was told its index was over the limit and rewrote it in the
  teach session: the new lesson first, 22 entries kept in place, and the rest
  moved into 16 topic files. That session took 9 turns and $0.20; the probes
  after it were cheap and correct.
- Claude Code does not hand its credential down to hooks: the brief's model
  call was refused as not logged in until the token was passed to the
  container under a second name.

This is one run per cell on three lessons. It says the tail can be found; it
does not yet say how often.

**Two faults this test found, both fixed before the numbers above:**

- Claude Code keeps 10,000 characters of what one hook adds and replaces the
  rest with a 2,000-character preview. The index had been raised to 24,000
  characters that morning, so with 265 lessons a session saw about a dozen.
  The first run of this test, before the fix, failed 4 of 6 cmem probes,
  including both in the arm whose index nominally held everything. Earlier
  results are unaffected: their memories fitted under the limit.
- git repacked the library in a background process after about a hundred
  commits, and the next write could fail with a missing object. Seeding 264
  notes failed 2 times in 8; with housekeeping kept in the foreground, 0 in 16.

## 2026-11-18, replacement by agents, and the consolidation routine

**With `cmem note --replaces ID` available** (ADR 0017), Sonnet 5.5, one run
per cell:

| Memory | Lessons | Wrote | Same repository | Other repository |
|---|---|---|---|---|
| cmem by hand | 3 unchanged, 2 changing | 5/5 | 5/5 | 5/5 |
| cmem with the hook | 3 unchanged, 2 changing | 5/5 | 5/5 | 5/5 |

- On all four changing runs the agent recorded the correction with
  `--replaces` on its first try. Memory ended with one current entry per
  lesson and no flags; before, it ended with two.
- The question "memory already holds something close" was never asked in
  these runs, because the agents named what they were replacing unprompted.
  That path is covered by tests and has not yet been seen in a real session.
- Seeding the messy library below without `separate` showed what the check
  catches: 4 of the 5 real pairs, missing the two deploy-freeze notes, which
  share few words. It held nothing it should not have among 30 entries.

**The consolidation routine** (`routines/consolidate.md`) on a library of 30
entries holding three replaced lessons, two pairs of duplicates, one flagged
entry nobody corrected, two pairs of look-alikes, and 16 unrelated lessons
(`mess.json`):

| Consolidator | Entries after | Changed | Checks | Est. cost |
|---|---|---|---|---|
| Sonnet 5.5, first run | 25 | 7 | 14/14 | $0.06 |
| Sonnet 5.5, second run | 26 | 5 | 14/14 (see below) | $0.04 |
| Haiku 5.5 | 25 | 6 | 14/14 | $0.005 |

- Every run retired the three stale entries, kept the new versions trusted,
  left the look-alikes separate, left the unanswered flag for the owner, and
  touched none of the unrelated lessons.
- Merged entries kept every detail and inherited the person's backing.
- The two deploy-freeze notes overlap but differ on a detail. Two runs merged
  them, keeping both details, and said so in the report; one left both and
  listed them for the owner. Either is defensible, so the check now only asks
  that no detail is lost. As first written it wanted a merge and failed the
  second Sonnet run.
- Haiku retired the vaguer TZ note as a duplicate where Sonnet merged the two.
  Nothing was lost, since the surviving note had every detail.
- Reverting the run restored memory exactly, in all three.
- Probes on the raw and on the consolidated library (three lessons, both
  repositories, cmem with the hook, Sonnet): 6/6 on each. Consolidated memory
  did as well as raw, with five fewer entries.

Three consolidation runs on one hand-made library are a first look. The
checks cannot tell whether a merged entry changed meaning; the merged texts
were read and had not.

## 2026-11-18, after the recall fixes, and the first changing cases (Sonnet 5.5)

**cmem by hand, re-run with the recall fixes: wrote 9/9, same repository 9/9,
other repository 9/9** (it was 9/9, 7/9, 6/9). Both fixes did the work:

- The soft-spread searches (`topologySpreadConstraints zones checkout`) now
  match the note directly, because compound words are split.
- All six pnpm probes still searched with words the note does not contain
  (`add dependency package javascript`). Each was shown what memory holds
  instead of "nothing matches", and used it. Here memory held one entry, so
  the listing was sure to include the lesson. In a large memory it would not
  be; that case is still the scale test's to answer.

**A lesson that changes** (two cases, one run per arm): the lesson is taught,
then a later session is corrected to a new version, then both repositories are
probed for the new version.

| Memory | Wrote the new lesson | Same repository | Other repository |
|---|---|---|---|
| Built-in, per repository | 1/2 | 1/2 | 0/2 |
| Built-in, one shared directory | 1/2 | 1/2 | 1/2 |
| cmem by hand | 2/2 | 2/2 | 2/2 |
| cmem with the hook | 2/2 | 2/2 | 2/2 |

What the transcripts showed:

- **Built-in memory handles a change cleanly when it writes at all.** For the
  renamed context it rewrote the one memory file in place (and renamed it),
  keeping a line on what the old name was. For pnpm-to-bun neither built-in
  arm wrote anything, at the first lesson or the second; the agent said "since
  your organisation uses bun" and saved nothing. Same failure as before: the
  lesson is not written.
- **cmem handled the change in all four runs the same way:** it flagged the
  old entry as outdated and noted the new one, whose text says what it
  replaces ("formerly devvm"). The probes then followed the new entry.
- **The cost of having no supersession for agents is clutter, not a wrong
  answer, so far.** After each change the library holds two current entries,
  one flagged. Every change adds an entry that will never leave the index.
  With one change per lesson the agent sorted it out; this has not been tried
  with several changes or in a crowded memory.

## 2026-11-18, three runs per cell (Sonnet 5.5, three cases, so 9 per cell)

| Memory | Wrote | Same repository | Other repository |
|---|---|---|---|
| Built-in, per repository | 5/9 | 5/9 | 0/9 |
| Built-in, one shared directory | 7/9 | 7/9 | 7/9 |
| cmem by hand | 9/9 | 7/9 | 6/9 |
| cmem with the hook | 9/9 | 9/9 | 9/9 |

Estimated cost was about the same for all four, $0.07 to $0.08 per case.

What the transcripts showed:

- **Every built-in failure was a lesson not written.** Whenever the lesson
  reached built-in memory, the same-repository probe passed, and so did the
  other-repository probe when the directory was shared. The soft-spread lesson
  was written in only 1 of 6 built-in teach sessions.
- **cmem wrote the lesson every time.** Its session text tells the agent in so
  many words to record what the user corrects; built-in memory relies on the
  model's own judgement. Adding that sentence to a CLAUDE.md would probably
  close the gap for built-in memory. That has not been tested.
- **The trust fix worked.** Every note was recorded as told by a person and
  stored as trusted; no session discounted a note.
- **Every by-hand cmem failure was a recall miss.** The agent searched with
  words the note did not contain (`topologySpreadConstraints zones checkout`
  against a note saying "topology spread constraints"; `add dependency package
  manager` against a note about npm and pnpm) and was told nothing matched.
  The hook arm does not depend on the search: the index is already in context.
- After these runs, recall was changed to split compound words and, when
  nothing matches, to show what memory holds. The re-run is in the section
  above.

The cmem arms' first run (below) used the earlier wording and is not counted
in the table above.

The two changing cases were validated the same day on Sonnet: both probes fail
with no memory and pass with the new lesson pre-written.

## 2026-11-18, the cases themselves

On all three cases the no-memory arm failed every probe and the arm with the
lesson pre-written passed every probe, on Haiku. The reworded soft-spread probe
was re-checked on Sonnet with the same outcome.

## 2026-11-18, which memory carries a lesson (Sonnet 5.5)

| Memory | Wrote | Same repository | Other repository | Input tokens per call |
|---|---|---|---|---|
| Built-in, per repository | 2/3 | 2/3 | 0/3 | 39.5k |
| Built-in, one shared directory | 3/3 | 3/3 | 3/3 | 42.0k |
| Shared directory holding 22 unrelated lessons | 3/3 | 2/3 | 2/3 | 36.6k |
| cmem by hand | 3/3 | 2/3 | 2/3 | 56.0k |
| cmem with the hook | 3/3 | 3/3 | 3/3 | 41.8k |

Neutral tasks (no lesson should change the answer; three sit next to a lesson):
6/6 with no memory, 6/6 with the crowded shared directory.

What the transcripts showed:

- Per-repository memory never carried a lesson to the other repository. In
  repository B the agent wrote a bare `kubectl` command, as in the incident
  this case is modelled on.
- cmem with the hook matched shared built-in memory, at the same token cost.
- cmem by hand cost about a third more tokens, from the extra recall turns.
- cmem by hand failed one case because of our own label. The agent recalled
  the note and wrote: "it's marked unverified, so I didn't rely on it". Every
  note in these runs was sourced with `--evidence`, never `--human`, although
  the lesson came from the user, so every note was unverified.
- In one case the agent noted its own first answer as a fact before being
  corrected, then noted the right rule and flagged the first note. The wrong
  note stays current; nothing can supersede it yet.
- The crowded directory's one failure was the probe's fault: it asked for a
  guarantee, which the lesson forbids, and the agent overrode the lesson and
  said so. The probe has been reworded since.
- Sonnet did not always write the lesson down unprompted with built-in memory
  (8 of 9 teach sessions across the built-in arms).

## 2026-11-18, the same test across models (kubectl case only)

| Model | Memory | Wrote | Same repository | Other repository |
|---|---|---|---|---|
| Haiku 5.5 | Built-in, per repository | no | fail | fail |
| Haiku 5.5 | Built-in, shared | no | fail | fail |
| Sonnet 5.5 | Built-in, per repository | yes | pass | fail |
| Sonnet 5.5 | Built-in, shared | yes | pass | pass |
| Opus 5.5 | Built-in, per repository | yes | pass | fail |
| Opus 5.5 | Built-in, shared | yes | pass | pass |

Haiku accepted the correction, fixed its answer and saved nothing, on all three
cases. With Haiku the limit is the writing, not the store.

## Estimated cost per session

About $0.002 on Haiku, $0.03 on Sonnet, $0.09 on Opus.

## Not yet tested

- More than one run at scale, and tasks no lesson should affect against the
  large memory (does a crowd of 264, or the brief, cause a lesson to be
  misapplied?).
- The brief on the owner's laptop, where the credential reaches Claude Code
  through a shell function a hook does not pass through.
- A lesson that changes more than once, and more than one run of the changing
  cases.
- Consolidation on a library that real use produced, and on one too large to
  read in one go.
- Built-in memory with a sentence in CLAUDE.md telling the agent to record
  corrections. cmem's advantage at writing lessons down may be that sentence.
- Weak models with something else doing the writing.
- Any harness other than Claude Code.
