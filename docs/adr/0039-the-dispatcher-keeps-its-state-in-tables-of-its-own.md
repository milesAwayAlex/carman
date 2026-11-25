# 0039: The dispatcher keeps its state in tables of its own, in a Postgres database it owns

Status: Accepted (2026-11-25; proposed 2026-11-24). The owner: "i scanned them, looks okay, let's test the system in practice".

## Context

The dispatcher keeps a durable log of events, starts work, waits for what
the work waits for, and notices what did not happen: an attempt that ran
too long, a wait whose deadline passed, a task not started by its time, a
schedule's missed ticks. All of it has to survive its process dying at any
moment (R6), and it has to explain why a task is not running (R7).

Before choosing how to build it, its behavior was written down as a
contract (`internal/dispatch`, the doc comments are the specification) and
an acceptance suite (`internal/dispatch/dispatchtest`) that kills instances
at every step of a task's life, restarts them as another build, and runs
two at once. Three implementations were then built against the same
contract and suite, by the same model, in one sitting:

- **Hand-written**: tables of our own in Postgres, written through `pgx`,
  with no workflow or queue library.
- **DBOS Transact**: durable workflows and its database-backed queue, beside
  our tables.
- **River**: a Postgres job queue, beside our tables.

| | Hand-written | DBOS | River beside our tables |
|---|---|---|---|
| Model cost at list price | $2.48 | $4.30 | $2.96 |
| Minutes | 12 | 17 | 14 |
| Lines of Go | 785 | 1,132 | 990 |
| Tables | 5 | 5 of ours + 13 | 5 of ours + 5 |
| Modules added | 6 | 22 | 17 |
| Suite where it was built, 5 runs | 5 pass | 5 pass | 5 pass |
| Suite over a slower connection | 6 of 6 pass | 8 of 11 pass | 0 of 6 pass |

The River failures were all one case, in which the contract and the suite
disagreed about the reason a pending task shows; the contract has since
been settled and the case made independent of timing. The DBOS failures
were restart cases in which a task was not done within five seconds of the
restart; they were not diagnosed.

**Why neither library made it easier.** Both had to be surrounded by the
same tables the hand-written build has, because the hard parts of this
contract are not the parts they offer.

- *DBOS* gave durable workflows, a queue with a global limit and priority,
  and messages with durable timeouts. The log and its order, the task table
  and its explanations, admission (its queue has no "not now, keep your
  place"), schedules (its cron cannot fire the latest missed tick once and
  count the rest), and late starts were still written by hand. Its
  open-source edition does not recover the work of another instance that
  died: that is its paid service's job, and the build reached an unexported
  method to do it. Recovery is pinned to an application version, so a new
  build does not carry on an old one's work unless the version is fixed by
  hand and every change to a workflow keeps replaying the same steps.
  Cancelling the context it is started with durably cancels every workflow
  in flight. It adds 13 tables and 87 migrations that change with each
  release, a waiting task holds a goroutine, and nothing removes its rows.
  It took the start loop and the limit off our hands and added a second
  state machine to keep in step with ours.
- *River* gave transactional enqueueing, a worker pool and a row per try.
  Its builder found that River "mostly sits beside my tables": replacing
  its job with a goroutine would have removed about 40 lines and changed
  nothing the suite can see. Its scheduled jobs are made available every
  5 seconds, too coarse for deadlines and 1-second ticks; a crashed job is
  rescued after an hour by default, so nothing whose recovery matters could
  rest on it; it has no global limit and no waiting for an event; its
  periodic jobs live in process configuration with no catch-up; and its
  migrations cannot run in one transaction.

What the contract needs most (an ordered log that a reader never sees out
of order, each transition recorded exactly once with its state change,
waits matched in log order, start claims that survive a crash, schedules
fired from the database's clock) is a few hundred lines of SQL in one
transaction each. A library that does not offer waiting for an event with
a deadline, and recovery of another instance's work, leaves all of that to
us, and adds its own state beside it.

## Decision

- **The dispatcher owns a Postgres database**, as the memory service owns
  its git repository. Nothing else writes to it; watchers append events to
  its log through the dispatcher's own write path.
- **Its state is in tables of its own**, written through `pgx`
  (`internal/dispatch/pgxdispatch`): the log, the tasks, the schedules, the
  instances, and the schema's versions. Every write takes one lock, so
  writes are serial, the log is numbered in commit order, and a transition
  and its event are in one transaction.
- **One contract and one acceptance suite stand between it and its
  callers.** `internal/dispatch` is the contract and
  `internal/dispatch/dispatchtest` the suite; an implementation that passes
  the suite can replace this one without its callers knowing.

## What we own as a result, and how each part is covered

- **Correctness across crashes**: the acceptance suite (restart at every
  step, a different build, two instances at once), and a test of the log's
  read order under concurrent writers.
- **Schema changes**: numbered migrations embedded in the binary, applied
  once each under an advisory lock; an old build refuses a newer schema,
  and a running one stops taking work when it sees one. The rules for what
  a migration may do while an old instance is alive are in the package's
  README. Tests: migrations applied once by instances starting together; a
  database made before migrations existed is upgraded with its work; work
  in every state at the previous version carries on at the newest with no
  transition recorded twice; an old build stops on a newer schema. The
  tests run the current code at an older schema, not the previous release,
  so tolerance of the new schema by the old release rests on the rules and
  on review.
- **Growth**: a retention period for finished tasks with their events and
  for events that belong to no task, and a period per event type for the
  latter (usage readings are kept apart from task history). Nothing that
  belongs to an unfinished task is removed. A test with a short period.
- **Idle cost**: an idle instance writes only a heartbeat every five
  seconds; it wakes on `LISTEN`/`NOTIFY` and sleeps until the next deadline
  or tick. Measured on 20 seconds idle: 725 commits and 52 rows updated
  before, 30 commits and 5 rows updated after. A test that an idle instance
  takes no lock and still reacts at once to an event from elsewhere.
- **Calls out**: the starter and admission get a deadline (30 seconds by
  default); a call that passes it counts as failed for the round and the
  loop goes on. A test with a starter and an admission that hang.

We also own its operation: backups of the database (as for the memory's
library, ADR 0038), its Postgres version, and the SQL that explains why a
task is not running.

## What would make us look again

- More than one instance doing work at once, for throughput rather than
  for a rolling restart: the single write lock serializes every transition.
- Volume beyond what one loop handles: thousands of transitions a second,
  or a log read back by type and time at a scale where retention and an
  index are not enough.
- A library that offers waiting for an event with a deadline, and recovery
  of a dead instance's work, in its open-source edition, with a schema we
  could live beside.
- The suite failing in a way that a fix to our SQL does not settle.

## Consequences

- About 1,300 lines of Go and 80 of SQL of our own, which the suite guards. An
  edit to a guard (the start claim's conditions, the wait's log order) can
  break it silently where the suite does not reach.
- A Postgres database to run beside the memory service, with its backups.
- The dispatcher's events are plain rows: the owner can read them with SQL.
