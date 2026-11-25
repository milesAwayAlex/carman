# 0043: The dispatcher starts work on time, says what did not happen, and reads the account's usage

Status: Accepted (2026-11-25; proposed 2026-11-25). The owner: "i scanned them, looks okay, let's test the system in practice".

## Context

The dispatcher (ADR 0042) started work only when asked by hand. The
scheduled agents were plain CronJobs, and when one did not run, or failed,
nobody was told: ADR 0036 left "an agent that is killed before it finishes
sends nothing" to something outside the agent. The owner asked for an
event system in which jobs that do not run or fail stop going unnoticed,
for visibility into the subscription's usage and limits as a first step
towards budgets, and for urgent work to come first and skippable routines
to wait (R7, R10).

The dispatcher stays a mechanical controller: no model is called inside it.

## Decision

**Schedules by time of day.** A schedule is an interval, as before, or a
five-field cron expression in a time zone (UTC unless named). The rule for
ticks missed while the dispatcher was down is unchanged: one task for the
latest, and one `schedule.missed` with how many were skipped. Ticks are the
instants at which the zone's wall clock reads a matching time: on the day
the clock goes forward a skipped time does not tick, on the day it goes back
a repeated time ticks twice. One package (`internal/dispatch/cron`) computes
ticks for the dispatcher's schedules and for its expectations of CronJobs,
so the two agree. An expression may have a sixth field, seconds, first:
Kubernetes takes only five, and the six exist so that the acceptance suite
can watch a cron schedule fire in seconds through the same code. The
contract gains `Undeclare` and `Schedules`.

**Schedules come from the values.** `dispatcher.schedules` lists them (name,
class, cron, time zone, how late a start may be, how long an attempt may
run, priority); an agent with `dispatched: true` adds its own; the usage
reading adds one. The service declares them at start and removes any it
has that is not listed. **One switch per agent**: `dispatched: true` both
declares the schedule and renders the agent's CronJob suspended whatever
else its entry says. The service refuses to declare, and the starter
refuses to start, a scheduled class whose CronJob is not suspended, so that
a CronJob unsuspended by hand is not started twice. A dispatched agent's
attempt may run ten minutes beyond its Job's own deadline, so that the
Job's failure, not an overrun, is what the owner hears of.

**What did not happen, for CronJobs the dispatcher does not start.** Every
pass (thirty seconds) the service reads each CronJob and every Job. A tick
is owed when the CronJob was not suspended when the service last looked
before the tick. A tick is kept by a Job of that CronJob for it, or for a
later tick within the grace (a cluster that catches up runs only the latest
tick it missed, as the dispatcher's own schedules do); a CronJob's Job is
named for its tick, and Jobs that the cluster has since removed are found
by their `job.ended` in the log. The grace is the CronJob's own catch-up
(`startingDeadlineSeconds`: a laptop asleep at the tick runs it on waking,
within that) plus `dispatcher.expect.grace` (an hour); the annotation
`dispatcher/grace` on a CronJob gives its own. A tick not kept is one
`run.missed`, whose ID names the CronJob and the tick, so it is recorded
once however often the service restarts. What has been judged is kept per
CronJob in the dispatcher's database (a new `marks` table, migration 4),
from when the service first saw it: earlier ticks are not judged.

**The owner is told, by a task.** A reader of the log, with its place kept
in `marks`, turns these into notices: a Job that failed (`job.ended`), a
`run.missed`, `task.not-started`, `task.overran`, `schedule.missed`. Each
is one task of class `notify` (a chart template that runs the
installation's `notify-owner` in the agent image with the words from the
payload), whose ID is derived from the event's, so that reading the log
again sends nothing twice. The dispatcher's image gets no way to reach a
person. A notice's failure is logged and not itself told. Each notice says
what, when, and the command to look with. **Off unless switched on**
(`dispatcher.notices.enabled`): off, each is recorded as `notice.withheld`
with the words it would have sent, so the owner can read a day of them
before switching on. On its first start the reader begins at the end of
the log: what happened before notices existed is not told.

**A program that tells the owner of its own failure says so** with the
annotation `dispatcher/notifies-owner: "true"` on its job template. Its
Job's failure is then not told again, if the program ended on its own
(there is an exit code and its deadline did not stop it): a program that
was killed could not tell anyone. The backup's job template carries it.

**Readings of the account's usage**, as events that belong to no task (kept
for `retentionUnowned`):

- `usage.observed`: every hour, the dispatcher's first real schedule runs
  `agent-usage`, which asks the cheapest model for one word with the
  harness's stream on, and records the last `rate_limit_event`'s
  `rate_limit_info` as it came, through the `agent` role (`dispatcher
  reading`, in the agent image). About a third of a cent a reading.
- `run.usage`: every `cmem routine` run records its agent, routine, the
  models tried, cost, attempts, token counts and the last rate-limit
  reading it saw (the harness now runs with its stream on). Recording never
  fails a run: no address, no token or a refusal is one line in its log.
  Agents and the dreamer are given the dispatcher's address and the
  `agent` token, which may add events to the log and nothing more.

`dispatch status` begins with the standing: the latest reading and its age,
when each window resets, and what runs cost in the last five hours and
seven days, by agent.

**Deferring what can wait** (off by default, `dispatcher.admission`): a task
whose priority is below a threshold is held while the latest reading has a
window at or above a utilization that has not reset, with a reason a person
can read ("the five-hour window is at 93% and resets at 14:30 UTC; this
task can wait"), and admitted when a later reading is under it or the
window has reset. A reading older than `maxAge` is unknown, and unknown
admits. The usage reading's priority is above the threshold.

## Consequences

- A CronJob suspended between the service's last look and its next tick
  shows that tick as one `run.missed`: the service cannot tell it from a
  run the cluster did not start. Looking every thirty seconds keeps that
  window small.
- Every CronJob that is not suspended is expected, including ones a person
  made by hand. A schedule the cron package cannot read is logged once and
  not judged.
- Expectations are kept per CronJob UID: a CronJob deleted and made again
  is new, and its earlier ticks are not judged.
- The reader of notices and the expectations act only in the service's
  process: with two instances for a moment (a rollout), both act, and the
  log's IDs keep each event and each notice task single.
- Notices queue behind the running limit like any task, at a high priority.
- The `rate_limit_info` format is the harness's, recorded as it came; the
  admission rule reads `unifiedWindows` from it and admits when it cannot.
- The admission rule is a threshold, not yet the curve to reset that
  DESIGN section 5 describes, and it acts only on tasks the dispatcher
  starts: the scheduled agents still on CronJobs are not held.
- Added 2026-11-25: a run the dispatcher starts for a message (ADR 0045)
  records `run.usage` too, as routine `message` with the message's id, and
  its attempt may run as long as its agent's Job plus the same ten minutes.
