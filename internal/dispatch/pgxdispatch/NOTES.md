# pgxdispatch: notes

These are the first builder's notes, kept as written. Since then (see
`README.md`): the schema is numbered migrations, not one file run at every
start; the log has retention; the three polling loops are one loop woken by
`LISTEN`/`NOTIFY` and by the next deadline, with a five-second fallback; the
heartbeat is every five seconds and the lease fifteen, an instance being
alive also while its listening session exists; calls to the starter and to
admission have a deadline; and the reason question under "Suite against
contract" is settled in the contract (admission's refusal stands while the
limit is reached).

The dispatcher on hand-written Postgres tables, through pgx v5, with no
workflow or queue library. It passes the whole acceptance suite unchanged,
restart, different-build and two-instance tests included.

## Design

**Tables** (`schema.sql`, embedded, run with `CREATE ... IF NOT EXISTS` at
every `Open` under an advisory lock so that two instances do not collide):

- `meta`: one row holding `seq` (the last Seq given) and `cursor` (the log
  has been acted on up to here).
- `events`: the log. `seq` primary key, `id` unique; `data` is `json`, not
  `jsonb`, so it comes back byte for byte.
- `tasks`: one row per task. Its life is columns: `state`, `attempt`,
  `started_at`, `wait`/`wait_type`/`wait_subject`/`wait_deadline`/`wait_after`,
  `wake` (what the next attempt is told), `not_started`, `starting_by` (a
  start claim), `reason`/`reason_detail` (for `Explain`).
- `schedules`: the declared schedule and `last_tick`.
- `instances`: one row per instance with a lease, `alive_until`.

**One write lock.** Every write transaction begins with
`SELECT ... FROM meta FOR UPDATE`. Writes are therefore serialized, which
is what makes the log safe to read: a Seq is taken under the lock and the
lock is released only at commit, after the transaction is visible, so no
transaction with a lower Seq can commit after one with a higher Seq. A
reader at Seq n never later sees a record at or below n
(`TestR2LogReadOrder` checks it with 8 concurrent appenders and a tailing
reader). The same lock makes every transition a plain serial program: no
deadlocks, no serialization retries, and an event and its state change are
always in one transaction, so transitions are recorded exactly once.

**Loops**, in each instance, every 100 ms (sooner after a `Submit` or
`Emit` through the same instance, an in-process hint; no `LISTEN/NOTIFY`):

1. *react*: under the lock, read events after `meta.cursor` in batches of
   200, act on each (a `task.finished` for the current attempt; then wake any
   waiting task whose wait matches and whose `wait_after` is below the
   event's Seq), and advance the cursor in the same transaction. Then, in a
   second transaction, the clock: overruns, wait deadlines, `StartBy`, and
   schedules. Time is the database's `clock_timestamp()`, taken once per
   transaction and used for event times too, so all instances share a clock.
2. *start*: read pending tasks in order (priority, then submission), call
   the limit and admission outside any transaction, then *claim* the task
   (`starting_by = me`, guarded by: still pending at the same attempt, no
   live claim by another instance, the limit still allows, and no
   `task.finished` for the attempt in the log), call `Start` with no
   transaction open, and record the outcome only if the claim still stands.
3. *heartbeat*: renew the lease every 400 ms; a claim counts while its
   instance's `alive_until` (2 s) is in the future. After a crash during a
   start, the next instance starts the attempt again once the lease runs
   out, under the same attempt name.

`Kill` cancels the instance's context (pgx closes the connection, so any
open transaction rolls back), waits for the loops, and closes the pool.
Nothing is marked: the lease just runs out. `build` is stored and ignored.

## What the library gave, and what I wrote

pgx gave a connection pool, context cancellation that really drops the
connection, transactions with `BeginFunc`, row collection helpers, and
multi-statement `Exec` for the schema. Everything else is hand-written: the
schema, the ordered log, the cursor, every transition, timers, schedules,
start claims and their lease, recovery (which is just "the loops read the
tables again"), and the test factory (a schema per test via `search_path`).

## Sharp edges hit

- `jsonb` would reformat the payload (`{"n":1}` comes back `{"n": 1}`), so
  `json` columns, and the payload kept apart from the spec so that
  `json.Marshal` does not compact it.
- `timestamptz` keeps microseconds, Go keeps nanoseconds: `Explain` must
  return the `Wait` as given (the suite compares with `Equal`), so the Wait
  is stored as JSON beside the columns used for querying.
- `time.Time.Truncate` counts from year 1, not the Unix epoch; ticks are
  computed from `Unix()` (the in-memory reference uses `Truncate`, which is
  right only for intervals that divide a day).
- **Suite against contract.** The README says a task held when the limit
  is reached shows `limit`. `R6_Restart/PriorityAndHoldsAsBefore` expects
  a held task to show `admission` *while another task runs at the limit*;
  the reference passes only because its test polls inside a 50 ms window.
  I keep the last admission reason when the limit is reached (admission was
  not asked again, so what it said last stands). A contract wording question
  for the owner, not a suite change.
- A narrow window remains: if a watcher emits `task.finished` for a pending
  attempt between the claim's commit and the `Start` call, `Start` is still
  called after the event. The contract forbids it; no test can hit it in
  practice. Closing it would need the claim and the call under one lock.
- Dispatcher event IDs are guessable: an outside event emitted with the ID
  `task.started/x/x#1` would be kept and the real transition dropped by
  dedup. The contract allows it; a production log should reject foreign
  events with `source = dispatcher`.
- Small environment ones: `go get` of pgx alone left `pgxpool`'s module
  missing until a second `go get`; `go test` here needs `CGO_ENABLED=0`.

## Worries for years of production, maintained by one person

- **The single write lock** caps throughput (each transition is a few ms
  of serialized work), and one slow statement in a react batch stalls every
  `Emit`. No `statement_timeout` or `idle_in_transaction_session_timeout` is
  set yet; a hung transaction would stop the whole dispatcher.
- **Idle cost.** Each instance runs about 25 write transactions a second
  even when idle; each `FOR UPDATE` dirties the `meta` row, so WAL and
  vacuum churn forever. A cheap unlocked check before taking the lock, or
  `LISTEN/NOTIFY` plus a longer poll, would fix it.
- **No migrations.** `CREATE TABLE IF NOT EXISTS` cannot evolve a column.
  The first schema change needs a version row and hand-written `ALTER`s that
  both builds of a rolling restart tolerate.
- **The log grows forever**; no retention or archiving.
- **Correctness lives in ~700 lines of my own SQL and Go.** It is short and
  serial, which helps, but every guard (the claim's five conditions, the
  `wait_after < seq` rule) is a place a future edit can break silently; the
  acceptance suite is the only safety net.
- **The lease length** trades restart speed (up to 2 s before a crashed
  start is retried) against double starts when an instance stalls longer
  than the lease (harmless only if starters stay idempotent by attempt name).

## Why is task X not running

```sql
SELECT t.state, t.attempt, t.reason, t.reason_detail, t.wait, t.start_by, t.not_started,
       t.starting_by, i.build, i.alive_until > clock_timestamp() AS claim_live,
       (SELECT count(*) FROM tasks WHERE state = 'running') AS running
FROM tasks t LEFT JOIN instances i ON i.id = t.starting_by WHERE t.id = 'X';
SELECT seq, time, type, source, id FROM events WHERE subject = 'X' ORDER BY seq;
SELECT seq - cursor AS unread_events FROM meta;           -- is the dispatcher behind?
SELECT id, build, alive_until FROM instances
WHERE alive_until > clock_timestamp();                    -- is any instance alive?
```

## Numbers

- Lines of Go that are not tests: 785 (`pgxdispatch.go` 352, `loop.go`
  433), 687 without blank and comment lines; plus 64 lines of SQL.
- Tables created: 5 (`meta`, `events`, `tasks`, `schedules`, `instances`).
- Modules added to `go.mod`: 6 (`jackc/pgx/v5` direct; `jackc/pgpassfile`,
  `jackc/pgservicefile`, `jackc/puddle/v2`, `golang.org/x/sync`,
  `golang.org/x/text` indirect).
- Suite: about 11 s wall clock. 5 of 5 runs passed on the final code (33
  tests each, none skipped), and 15 of 15 in a stress run before it.
