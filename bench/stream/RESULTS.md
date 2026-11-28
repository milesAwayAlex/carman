# Task-stream benchmark: results so far

What the runs of the stream showed (M7; the design and the conditions
written before any run are in `docs/adr/0069-task-stream-benchmark.md`).
Every number is from what the agent did not write: the world's verdict,
the memory service's log, the runs' own count of cost (which matched the
meter's exactly where the two were set side by side). Raw results, world
states, libraries and logs are under `$CARMAN_BENCH_ROOT/stream/` on the
machine that ran them, not in this repository.

All runs: the world `stream` (forty tasks over five days), Claude Sonnet
5.5, Claude Code 2.1.326, each task a fresh session of a trial's agent held
to the world's commands. Arms: **none** (no memory), **built-in** (Claude
Code's own memory, in a directory kept between tasks), **notes** (cmem
with its hook, filled by the sessions), **tidied** (notes, and the
dreamer's two routines between days).

## 2026-11-28, the first counted run: three rounds, read on 33 tasks

Four arms, three rounds, 480 sessions, all ran. $12.78 by the runs' own
count. Seven tasks were set aside before rounds 2 and 3 were seen (s10,
s16, s20, s25, s29, s33, s37, the deploys to prod): they failed in every
arm because Sonnet does not approve a deploy in a person's name and the
tickets did not say the approval was given. On the other 33 (99 tasks an
arm):

| | none | built-in | notes | tidied |
|---|---|---|---|---|
| Tasks passed, of 99 | 66 | 98 | 86 | 95 |
| A lesson needed again followed, of 87 | 21 | 85 | 75 | 82 |
| Refusals each time a known rule was needed | 0.48 | 0.02 | 0.01 | 0.02 |
| Neutral tasks passed, of 30 | 30 | 30 | 30 | 30 |
| Calls that followed an old rule after it changed | 0 | 3 | 3 | 3 |
| Cost a task | $0.014 | $0.026 | $0.025 | $0.033 |
| Cost a passed task | $0.022 | $0.026 | $0.029 | $0.034 |

Against the conditions written before the run:

- **Memory helps, by far.** Every arm with memory passed 20 to 32 points
  more than the arm with none, and ran into a rule it had met about one
  time in fifty where the arm with none did one time in two.
- **It is not cheaper.** A passed task cost more in every arm with memory
  than with none. The tasks are short; a session with memory reads more
  and writes notes.
- **No harm on neutral tasks**, and after a rule changed the old one was
  followed equally rarely by built-in and by cmem.
- **M7 is not met on this run.** The notes arm passed 12 points less than
  built-in, at about the same cost a task; the tidied arm 3 points less,
  at a quarter more. Three runs separate arms about fifteen points apart:
  twelve is at the edge of that, three is inside it. So cmem with nothing
  tidying it is likely worse than built-in memory here; cmem with the
  nights is not shown to differ from it in success, and costs more.
  Neither arm is above built-in.
- **The nights**: tidied passed more than notes (95 against 86) and cost
  more a passed task. A lean, not a finding.

**Where the cmem arms lost.** Every rule of the world that is met by
doing was carried by all three memory arms alike. The difference is the
two things the owner tells once, on day 1:

| Followed later | built-in | notes | tidied |
|---|---|---|---|
| The line every release note ends with, of 9 | 9 | 3 | 6 |
| A summary attached with every change, of 15 | 15 | 10 | 15 |

In every failure the session had been given the entry that holds the
instruction, in time (the memory service's log: "had it, did not follow"
is 12 of 87 for notes and 5 for tidied; "did not have it" is none). Two
things went wrong:

- **An entry's id written into the work** (4 release notes, all in round
  1's notes arm): the session followed the rule and then cited the entry
  after it, `Reviewed-by: platform (20261128-015933-logl)`, so the line
  was no longer exactly what was asked.
- **The instruction not acted on** (6 release notes, 5 summaries): it was
  one of thirty-odd one-line entries shown as the session began, and the
  work was done without it.

Built-in memory wrote one or two files that it kept revising, and missed
neither instruction once.

What this run cannot say: anything about stale or wrong memory of who
approves (the tasks set aside); that built-in and tidied differ; anything
beyond this made world, one model and one harness.

Deviations, as they happened:

- A first round on Sonnet, before this run, is a pilot and is not counted
  (the stream was changed after it: `--help` no longer carries a command
  out, and the help says whose it is to push a version). After that
  change every session pushes first, so the image rule measures nothing.
- The runner changed during the run (it reads a run's usage more than
  once before taking a session not to have run); nothing a trial is built
  from changed.
- Round 2 stood idle for half an hour: one read of a trial's log came back
  empty and the runner took a session that had run for one that had not.
  That run was gone on with in its trial, and its one task's row (round 2,
  built-in, s04) was recorded by hand from the trial's log.
- Round 2's built-in arm ran from its fifth task beside the notes and
  tidied arms, not beside the arm with none; those two had a third trial
  on the node.

## 2026-11-28, the second run: one round on all forty tasks

The same stream with one change: each ticket for a deploy to prod ends
"The service's owner has approved this deploy." The stream is frozen
after that change. Three rounds were planned; it was stopped by decision
after one, since the memory arms were at the ceiling and two more rounds
could not separate them. **One run an arm**: read it as that. $4.12.

| | none | built-in | notes | tidied |
|---|---|---|---|---|
| Tasks passed, of 40 | 29 | 40 | 39 | 40 |
| The seven deploys to prod passed | 7 | 7 | 7 | 7 |
| A lesson needed again followed, of 52 | 9 | 50 | 51 | 48 |
| Refusals each time a known rule was needed | 0.83 | 0.04 | 0.02 | 0.08 |
| Calls that followed an old rule after it changed | 0 | 1 | 1 | 1 |
| Calls that followed a wrong note | 0 | 0 | 1 | 1 |
| Neutral tasks passed, of 10 | 10 | 10 | 10 | 10 |
| Cost a passed task | $0.024 | $0.027 | $0.026 | $0.033 |

- The change did what it was for: every deploy to prod passed in every
  arm, the arm with none paying four to seven wrong calls for each and
  the arms with memory none after the first.
- The stream no longer tells the memory arms apart on success: 40, 39 and
  40 of 40. A stream that could would need tasks a session with memory
  still fails.
- Harm is small and alike: one call in each memory arm followed a rule
  that had changed; the wrong notes were followed in one call each by the
  two cmem arms and in none by built-in. One call is not a difference.
- In this round the notes arm carried both things told once (it failed
  one task of forty), which the first run's three rounds show is not
  reliable.

## What the two runs say together

Memory, of either kind, is worth a great deal on work that repeats its
rules. On this stream cmem does not beat Claude Code's built-in memory:
without the nights it is likely behind, with them it is level in success
and dearer. What it loses on is not recall (the lesson was in front of the
session nearly every time) but acting on an instruction given once, and
its own habit of citing entries in the work. Both are named on the
improvement list.
