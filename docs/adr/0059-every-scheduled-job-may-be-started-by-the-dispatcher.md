# 0059: Every scheduled job may be started by the dispatcher, and places are kept for it

Status: Proposed (2026-11-27).

## Context

ADR 0043 lets the dispatcher start work on schedules: a class whose
CronJob is a suspended template, started at each tick, recorded, a missed
tick caught up once with `schedule.missed`. An agent says so with
`dispatched: true`, and the usage reading is started that way. The
dreamer, the nightly backup and the warm image's job were still started by
their own CronJobs, so two things started work, and the dispatcher only
observed most of it (`job.ended`, `run.missed`).

The installation runs at most `dispatcher.maxRunning` tasks at once (three
on a real one). A task a message starts (a session, priority 50) can run
for hours. Priority orders what is pending; it does not free a place. With
every place taken by long work, a nightly job waited until one ended.

## Decision

**Each scheduled job of the chart has a value that moves it to the
dispatcher**: `dreamer.dispatched`, `backup.dispatched`,
`agent.warm.dispatched`, and an agent's `dispatched` as before. Its
CronJob stays, suspended, as the template; the dispatcher is given a
schedule of the same name as its class (`dreamer`, `backup`, `agent-warm`)
with the CronJob's cron expression and time zone, and refuses to start a
scheduled class whose CronJob is not suspended (ADR 0043).

For each, when the dispatcher starts it:

- **Time zone**: UTC, as the CronJobs had it, so that the ticks are the
  same instants before and after the move.
- **Catch-up**: the job's `catchUpSeconds` is the schedule's
  `startWithin`. A tick missed while the dispatcher was down is started
  once when it is back, for the latest tick, with one `schedule.missed`
  saying how many were skipped. Unlike a CronJob's
  `startingDeadlineSeconds`, a start later than `startWithin` is not
  dropped: it is recorded and told as `task.not-started`, and the task still
  runs when a place frees.
- **Deadline**: the Job's `activeDeadlineSeconds` is the job's
  `deadlineSeconds` (the warm job's is now a value, 1800 as before); the
  dispatcher's attempt timeout is ten minutes beyond it, so that the Job's
  failure, not an overrun, is what is told.
- **`waitForLimitMinutes`** (agents only) keeps its meaning: the run waits
  in its pod for the plan's limit to reset, and the deadline has to allow
  for it. Started by the dispatcher, the waiting pod holds a place all that
  time. With the admission rule on, a task whose ladder starts with a
  refused model is held pending instead (ADR 0052), which holds no place;
  so a dispatched agent should not set it once admission is on.
- **Priority**: the backup, the warm job and the usage reading use no
  model and have priority 100; the dreamer 60, above the admission rule's
  threshold (50), so that a high window does not starve it night after
  night (its own budget bounds it, and a refused first model still holds
  it); a weekly agent its own `priority`, 0 unless given.
- **Order**: the backup runs after the dreamer by clock times only (10:17
  and 10:47 UTC, the dreamer's deadline thirty minutes). The dispatcher has
  no dependency between tasks, so no order is stated. A dreamer started
  late may overlap the backup; the backup then copies the library as it
  stands (each note is written once, and the copy is a commit of its
  history), and the rest of that night's tidying is in the next night's
  copy.

**Places are kept for scheduled work** (`dispatcher.reserve`): of
`maxRunning`, `places` are kept for tasks of priority `from` (60) or more.
Work below it takes at most `maxRunning - places`; it is held with a reason
that says so, as admission holds it, while that many below `from` run.
`places` is fewer than `maxRunning`, and 0 (the default) keeps none. With
three places and one kept, two long sessions can run, and a nightly job
starts on time beside them.

**The dreamer's switch.** `dreamer.suspend` was the CronJob's own `suspend`,
read back from the cluster by each deploy. A dispatched dreamer's CronJob is
always suspended, so the switch is kept in its annotation
`dispatcher/scheduled` (`on` or `off`), which a deploy reads back instead;
off, the dispatcher is not given its schedule. `dreamer on|off` sets the
annotation, and the next deploy gives the dispatcher the change (the
dispatcher reads its schedules when it starts).

## The night of the switch, and back

Moving a job is one deploy with its value set: the same apply suspends the
CronJob and restarts the dispatcher with the schedule. A schedule's first
tick is the first after the dispatcher declares it, so a deploy made between
two ticks neither runs a tick twice nor skips one. A deploy across a tick
can lose that tick (the CronJob suspended before it, the dispatcher
declaring after it); so the deploy is made well clear of the job's times
(not between 10:00 and 11:30 UTC for the nightly jobs, nor at minute 41 of
hours 0 and 12 for the warm job).

Going back is a deploy with the value false: the dispatcher undeclares the
schedule, and the CronJob is no longer suspended. **A CronJob unsuspended
starts at once its latest tick within `startingDeadlineSeconds`** (seen in
a trial: unsuspended at 12 seconds past a minute, it started that minute's
run, which the dispatcher had already started). So going back runs the
latest tick a second time when it is within the job's catch-up. To avoid
it, go back more than `catchUpSeconds` after the latest tick (for the
dreamer and the backup, between 22:47 and 10:17 UTC), or set
`catchUpSeconds` to a minute for the deploy that goes back and restore it
on the next. The warm job's second run is harmless: it rebuilds only on a
difference.

## Consequences

- Every run of the chart's scheduled jobs is a task in the dispatcher's
  log, with its start, its end, a late start and a missed tick; the
  expectations of ADR 0043 are left for CronJobs a person adds.
- A Job the dispatcher starts is not owned by the CronJob, so the CronJob's
  history limits no longer prune it; its `ttlSecondsAfterFinished` does.
- `scripts/deploy.sh CONTEXT dreamer run` and `warm run` still make a Job
  from the suspended CronJob directly; the dispatcher records its end.
- A trial (`TRIAL_TICK=1 scripts/trial.sh CONTEXT up`) has a class `tick`
  that does nothing, on a schedule of every minute, to see the above.
