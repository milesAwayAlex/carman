# 0062: A task may start after another, and admission reads the ladder a job starts with

Status: Proposed (2026-11-27).

## Context

Two items of the list of improvements (ADR 0058).

`tasks-can-follow-another`. The dispatcher has no order between tasks
(ADRs 0039, 0043). The nightly backup follows the dreamer only by clock
times half an hour apart (ADR 0059): when the dreamer runs long, or starts
late, the backup copies the library half tidied.

`admission-reads-a-routines-own-ladder`. Admission holds a task whose first
model is refused (ADR 0052) and paces by the windows its first model draws
on (ADR 0060). It read the class's ladder, its CronJob's
`CMEM_ROUTINE_MODELS`. A routine may have a ladder of its own
(`CMEM_ROUTINE_MODELS_<ROUTINE>`, the dreamer's `modelsFor`), which the job
uses instead and admission did not read, so it judged by a model the job
does not start with.

## Decision

### After

**A task may name a class it starts after** (`TaskSpec.After`, in the
contract, tested in `dispatchtest` for every store). Its first attempt does
not start while a task of that class **submitted before it** is pending,
running or waiting.

- **What it waits for**: the end of that task, **however it ended**: done,
  failed, overran. The dreamer failing is told by its own notice; the copy
  of the library as it stands is still worth taking, and a backup that
  waited for a success would skip the nights it is most wanted. A
  condition on the other's outcome is not built.
- **Which task**: only those submitted before it. One of that class that
  never ran (not dispatched, switched off, not due that night) holds
  nothing, and neither does one submitted later: the task starts as any
  other. So **a cycle cannot hold**: of two tasks each after the other's
  class, the one submitted first does not wait for the later. A task after
  its own class waits for the previous run of its class to end.
- **How long**: at most `AfterWithin` (required with `After`) from its
  submission. Then it is let go: the dispatcher records one
  `task.after-lapsed` naming the task still open, and the task starts as
  any other, subject to the limit and admission. Running anyway, rather
  than giving up, is for the backup: a copy taken beside a dreamer is
  better than none, and the lapse is in the log.
- **What a person sees**: `dispatch status` shows the held task pending
  with the reason `after`: `after dreamer@2026-11-28T10:17:00Z, which has
  not ended (waits at most 40m0s from its submission)`. It is checked
  before the limit and admission, holds no place, and does not hold back
  the tasks after it in order.
- **A late start of both**: schedules that fire in one round (the
  dispatcher was down over both ticks) are submitted earliest tick first,
  so the backup still follows the dreamer.

**In the values**: `after: NAME` on a schedule (`dispatcher.schedules`), an
agent with `dispatched: true`, or the backup, names a schedule (or, if none
has the name, a class); `afterWithin` is a Go duration, by default that
schedule's timeout (its Job's deadline and ten minutes), or an hour. The
backup has `after: dreamer` by default; it counts only once both are
dispatched. A submitted task: `dispatcher submit CLASS --after CLASS
[--after-within 1h]`.

**Refused**: this is one order between two jobs, not a workflow engine.
No lists of tasks to wait for, no condition on how the other ended, no
passing of output from one to the other, no fan-out or join, no retry of
the other, no graph checked for cycles (none can hold, above).

### The ladder a job starts with

Admission judges a task by **the ladder its Job starts with**: that of the
first routine it runs (the CronJob's first argument, or the payload's when
the CronJob has none), `CMEM_ROUTINE_MODELS_<ROUTINE>` (capitals, `_` for
`-`, as `run-routine` reads it) where the container sets one, and else
`CMEM_ROUTINE_MODELS`. A run for a message (`TASK_MESSAGE`) runs no
routine and starts with the class's ladder, as `agent-resume` does.

For a class that runs several routines with different first models (the
dreamer: consolidate, then distill), **its model is the first routine's
first model**: that is what the job calls first and what a refusal stops
at once. The later routines' models are not looked at; a refusal there is
met by the run's own ladder (ADR 0029), and holding the whole job for its
last routine's model would hold the first's work too.

## Consequences

- With the dreamer and the backup dispatched, a night's copy has that
  night's tidying, unless the dreamer runs past its deadline (then the
  backup starts after `task.after-lapsed`, or after the dreamer's overrun).
- The held task's wait counts toward its `startWithin`: a long wait may be
  told as `task.not-started` too.
- `TRIAL_AFTER=1 scripts/trial.sh CONTEXT up` has two classes a minute
  apart, the second after the first, to see it; `TRIAL_AFTER_SLEEP` and
  `TRIAL_AFTER_EXIT` say how long the first runs and how it ends.
- The schema is unchanged: the store reads the submission time and the
  lapse from the log.
