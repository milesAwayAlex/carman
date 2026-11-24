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
