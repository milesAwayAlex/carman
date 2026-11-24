# pgxdispatch

The dispatcher on tables of its own in Postgres, written through pgx. The
contract is `internal/dispatch`; this file says how to run and change this
implementation. `NOTES.md` is how it was built and what its builder worried
about.

## Changing the schema

The schema is the numbered files in `migrations/`, embedded in the binary.
`Open` applies the ones the database has not had, in order, each in one
transaction with its row in `schema_versions`, all under one advisory lock:
instances starting together take turns, and each migration is applied once.
`0001_initial.sql` is the schema as it was before migrations existed, so a
database made then is upgraded like any other.

A released migration is never edited; a change is a new file with the next
number. Migrations go forward only: there is no down migration, and a
rollback is a deploy of a build that knows the schema.

**An old build never works on a newer schema.** `Open` fails with
`ErrSchemaNewer` when the database is at a version the build does not know.
An instance already running notices at its next heartbeat (every few
seconds), stops taking work, and reports it through `Err()`; it still has to
be killed, which its process does by exiting.

That is the rolling restart seen from the old instance: the new one migrates,
and for up to one heartbeat the old one goes on with its own statements
against the new schema. So a migration **may**:

- add a table, an index, or a column that is nullable or has a default;
- add a column the old build does not write, provided the new build gives
  the rows the old build writes a meaning (a backfill the new build repeats
  until the old one is gone; `0002` does this for `tasks.ended_at`);
- add data the old build ignores.

It **must not**, in the same release:

- drop or rename a table or column the old build reads or writes;
- change what an existing column means, or its type;
- add `NOT NULL` without a default, a `CHECK`, a unique index or a foreign key
  that rows the old build writes could break;
- take a lock for long: the old instance's transactions wait on it (an index
  on a large table is built `CONCURRENTLY`, which cannot run in a
  transaction, so it needs a migration of its own kind; none does yet).

Removing something is two releases: the first stops using it, the second,
once no instance of the first's predecessor can be alive, drops it.

The tests stop the migrations at a version (`Options.schemaVersion`) to run
an instance on an older schema. That instance runs today's code restricted to
that version's columns, not the previous release's code, so a migration's
test shows that work written at the old version is carried on, not that the
old release itself tolerates the new schema: that part is the rules above,
and review.

## Retention

`Options.Retention` bounds the log; zero keeps everything. The instance that
leads (the live one with the lowest id) removes, in statements of at most
100 tasks or 1,000 events that do not take the write lock:

- finished tasks (done or failed) whose end is older than the period, with
  their events (those whose subject is the task's ID);
- events that belong to no task (their subject is no task's ID: a usage
  reading, a `schedule.missed`, an event nobody waited for) appended longer
  ago than the period.

It never removes anything that belongs to a task that is not finished,
however old, nor an event the dispatcher has not yet acted on. An ID that
retention removed is free again: submitting a task or emitting an event with
it a second time has an effect. The contract's "one ID, one effect" holds
within the period, so the period must be longer than any watcher may retry
or repeat an event. Event `Seq` numbers keep increasing; removal leaves gaps.

**One period, or one per event type.** One period is right for task history:
it is read to explain what happened and nothing reads it later. It is wrong
for the log's other planned use, readings of the account's usage over time
that a scheduler reads back by type and time: those are worth keeping longer
than task history (to see a week's pattern, or a month's), or shorter when
they come often. So `Options.RetentionByType` sets a period for events of a
type that belong to no task, zero keeping them for ever, and the rest use
`Retention`. It cost a map and one statement per listed type. Retention
counts from appending (`events.added`), not from `Event.Time`, which the
emitter sets. When the scheduler is built, it will want an index on
`(type, time)` and a read by type and time range in the contract; neither
exists yet.

## Being idle

An instance runs one loop. A round reads `meta` and takes the write lock
only when the log has events not yet acted on, acts on the clock only when
something is due, offers pending tasks, and then reads when the next thing
is due: an attempt's limit, a wait's deadline, a start-by time, a schedule's
tick. It sleeps until then, or until a notification, or for at most five
seconds (the fallback for a lost notification). A write that appends to the
log, or declares a schedule, sends `NOTIFY` on a channel named after the
schema, from whatever instance or connection made it (the watchers' `Emit`
included); every instance `LISTEN`s on a session of its own.

While a task is held by admission or its start failed, it is asked again
every second; while another instance's start claim exists, it is looked at
every half second. Neither writes unless the reason changes.

The heartbeat, every five seconds, is the only write of an idle instance.
An instance counts as alive while its lease (fifteen seconds) runs **and**
its listening session exists in `pg_stat_activity`: a process that dies
closes its connection, so its start claims end at once, and the lease only
covers a connection left half-open (a node lost). This needs every instance
to connect as the same role, or a role with `pg_read_all_stats`, to see the
others' sessions. A session lost and reconnected gets a new identity, and
for that moment the instance's own claim looks dead: a start may then be
called twice, which the contract already allows.

Measured on one idle instance for 20 seconds (`TestIdleCost`, run with
`DISPATCH_MEASURE_IDLE=1`): before, polling every 100 ms, 725 commits, 52
rows updated, 127 kB of WAL; after, 30 commits (all but the heartbeat's
read-only), 5 rows updated, 1.2 kB of WAL. No rows inserted in either.

## Calls out

`Options.CallTimeout` (30 seconds when zero) limits each call to the
`Starter` and to `Admission`. The call gets a context with that deadline; if
it has not returned by then, it counts as failed for the round (a start
failed, `Explain` says `start-failed`; or admission refused, `admission`,
with "did not return in time" as the detail), the loop goes on with the next
task, and the call is made again a second later. A call that ignores its
context is left running in the background and its answer dropped, so a
starter must still be idempotent by attempt name: the hung call may yet
start the work, and so may the next.
