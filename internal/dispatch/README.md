# The dispatcher's contract and acceptance suite

`dispatch.go` states what a dispatcher does; read its doc comments, they are
the specification. `dispatchtest` is the acceptance suite every candidate
must pass unchanged. `memory` is an in-memory implementation that proves the
suite, and is a reference for the behavior where prose is ambiguous.

## Plugging in a candidate

Implement `dispatchtest.Database` and run the suite in one line:

```go
func factory(t *testing.T) dispatchtest.Database {
	url := dispatchtest.DatabaseURL(t) // skips the test when unset
	// create a fresh schema (or database) for this test, register
	// t.Cleanup to drop it, return something that opens instances on it
}

func TestAcceptance(t *testing.T) { dispatchtest.Run(t, factory) }
```

- `Open(cfg, build)` starts an instance on the test's schema. It works from
  the moment it returns. `build` is a version string (see "Restart" below).
- `Emit(ctx, e)` appends to the log *with no instance acting on it*: it is
  how the suite puts a watcher's event in while the dispatcher is down. It
  must be the same write path as `Dispatcher.Emit`, without the reaction.
- `Instance.Kill()` is described below.

Use `Run`, never `RunWithoutRestart`: that one exists only for `memory`.

## The database

The suite reads `DISPATCH_TEST_DATABASE_URL` through `DatabaseURL(t)`, which
skips the test when it is not set, so `scripts/test.sh` stays green on a
machine without Postgres. What to do with the address is the factory's
business. Tests run in parallel (`t.Parallel`), about 30 at once, so the
factory is called concurrently and each call must get its own clean schema;
mind the connection pool size.

The whole suite takes about 15 seconds of wall clock on the in-memory
store; it is mostly waiting, so a database should add little.

## What "restart" means

`Kill` stands in for the process dying. It must:

- run no graceful shutdown path: nothing is flushed, nothing is marked as
  stopped, no lease is released. Work in flight is abandoned; an open
  transaction is rolled back (cancel its context, drop its connection),
  never committed.
- after it returns, make no further call to the database, the `Starter` or
  `Admission`. The suite's fake starter fails the test if a killed instance
  calls it.
- it may wait for the instance's goroutines. A `Start` call in progress
  returns on its own once `Kill` is called (the suite unblocks it), so
  waiting does not deadlock.

The next `Open` on the same schema is the restarted process. It is given a
fresh `Config` with the same fake starter and admission. The suite kills at
these points: right after a submission; during a start call (the work was
started, the dispatcher never heard back); during a start call with the
watcher's finished event arriving while down; right after a start returned;
right after a finished event (done, waiting) was emitted; right after a wake
event was emitted; right after a second attempt started; and with work in
every state at once. In each, the task must end with exactly the transitions
it would have had without the crash.

**Different build.** `R6_RestartAsDifferentBuild` restarts with
`build = "build-2"` after `"build-1"`. It must behave like any other
restart: work begun by the old build is carried on by the new one. A
library that pins recovery to an application version (DBOS does) has to be
configured or wrapped so that it does not.

**Two instances.** `R6_TwoInstances` runs two instances on one schema at
once. Together they start each attempt once and record each transition
once; work submitted through one is visible and finished through the other.

## Decisions the brief left open

- **Attempt names are fixed by the contract**: `task#n`
  (`dispatch.AttemptName`). The number goes up only when a waiting task is
  woken. There is no automatic retry: failed is final, so is overran.
- **A finished event names its attempt** (`Finished.Attempt`). Otherwise a
  late or repeated finished event of attempt 1 could end attempt 2. One for
  any attempt but the current one, or for a task that is waiting or final,
  is ignored.
- **A finished event for a pending task's next attempt is acted on**, and
  `task.started` is recorded before the outcome. After a crash during a
  start, the dispatcher cannot know whether the work began; the watcher's
  event is the evidence, and the attempt must not be started again. (The
  suite found this in the reference implementation:
  `DuringStartFinishedWhileDown`.)
- **"Never start a finished attempt" is checked as**: no `Start(a)` begins
  after a `task.finished` for `a` was emitted. Before every start call,
  including the repeat after a restart, look for one in the log.
- **Starter errors**: the attempt did not start; the task stays pending and
  the same attempt is tried again later, through the limit and admission.
  `Explain` shows `start-failed`. No backoff is required; not tested.
- **A wait is matched in log order.** A matching event wakes a wait when it
  comes after, in the log, the `task.finished` that asked for the wait. This
  is what makes "an event that arrives before the wait is registered still
  wakes it" precise, and keeps old events from waking new waits. Any event
  matches, the dispatcher's own included (a task can wait for another's
  `task.done`).
- **The dispatcher's events have fixed IDs**: `type/subject/key`
  (`dispatch.TransitionID`), the key being the attempt name, the tick, or
  empty. Emitting one twice is then harmless by the log's own dedup, and the
  suite checks both the ID and that no transition appears twice under any
  ID.
- **An overrun records two events**, `task.overran` (the notice) then
  `task.failed` (the transition), in one step. Every failure, whatever its
  cause, is a `task.failed`.
- **`StartBy` applies to the first attempt only**; the attempt timeout to
  every attempt, counted from its `task.started`.
- **A task held by admission does not block the tasks after it.** The next
  in order is asked. A budget rule holds backlog work and lets urgent work
  through; head-of-line blocking would stall both.
- **The limit is checked before admission**; when the limit is reached,
  admission is not asked.
- **A pending task's reason is what happened the last time it was
  considered.** Pending tasks are considered in priority order whenever a
  place is free. A task admission refused shows `admission`, with
  admission's reason, until it is considered again; a reached limit does
  not replace it, because "the limit is reached" would say that the task
  starts when a place frees, which is false. A task never refused shows
  `limit` while the limit is reached.
- **Duplicate submissions: the first spec wins**, no error. Same for events.
- **Schedule ticks are multiples of the interval since the Unix epoch**, so
  every instance computes the same ticks with no stored anchor; the first
  tick fired is the first after declaration; intervals are whole seconds.
  **A cron schedule's ticks** are the instants its zone's wall clock reads a
  matching time (`internal/dispatch/cron`); `Schedule.Due` gives both forms
  the same catch-up. The suite uses the six-field form (seconds first) to
  watch one fire in seconds. Re-declaring with a different timing starts
  the new timing from the declaration: its ticks before it were never due.
  `Undeclare` removes a schedule; declared again, it is new.
  The scheduled task's ID is `name@tick` (RFC 3339), which makes firing the
  same tick twice a duplicate submission. Re-declaring replaces the interval
  and template and keeps what has fired. Schedules are durable: a restarted
  dispatcher keeps firing them without being told again.
- **No events for admission holds.** They are not transitions, and an
  admission asked every second would flood the log. `Explain` gives the
  reason instead.

## Left to the implementation

- How often it polls, as long as it reacts within about a second; the suite
  waits up to five. Schedules must be looked at more often than once a
  second, or a 1 s schedule will report missed ticks while running.
- The order in which tasks of equal priority and equal submission time
  start; the order of events written in one step, beyond what the contract
  states.
- Whether `Event.Time` comes from the database clock or the process clock.
  The suite tolerates 250 ms between its clock and the implementation's.
- Gaps in `Seq`.
- How two instances avoid starting twice (row locks, advisory lock, leader
  lease). A lease is fine as long as a killed instance's lease does not stop
  the next instance for longer than the suite waits (a few seconds).

## Where a candidate will have to think

- **The log's read order.** `Events(after)` must never later show a record
  with `Seq` at or below one already shown. A Postgres sequence alone does
  not give this: a transaction that took seq 10 can commit after one that
  took seq 11, and a reader at 11 misses 10. Waits are matched in log order,
  so this is also correctness, not only reading. Serialize appends (a lock
  on the log's tail), or number them at commit.
- **A start call is outside any transaction.** Between "decided to start"
  and "recorded that it started", a crash leaves the dispatcher unsure; the
  contract resolves it with idempotent attempt names and by trusting the
  watcher's finished event.
- **Exactly-once transition events** come for free if the event is written
  in the same transaction as the state change. With a workflow library, a
  step that both changes state and emits must not be replayed into a second
  emit.
