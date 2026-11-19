# 0015. Memory is stored as markdown notes in a git repository

Status: Accepted (2026-11-18). Supersedes 0008 and the Postgres half of 0007.

## Context

The first build stored memory in Postgres. The owner then asked whether an
interlinked markdown library would do instead: something both agents and
people can read, and still search.

What weighed for it:

- Benchmark research (ADR 0013) found that when memory hurt an agent it was
  mostly because stored records were noisy, not because search ranked badly. A
  store a person can read and review in an editor makes that visible.
- It is the format of Claude Code's built-in memory, which is the bar for M7.
- Git already provides what M2 and M4 ask for: history, an audit trail, review
  of a change as a diff, and revert.
- There is no database to host or back up; a git remote is the backup.

What Postgres did better: concurrent writers, ranked search built in, and rules
enforced even against direct access to the store.

## Decision

- The library is a directory of markdown notes in a git repository: entries,
  episodes and flags, each one file with a YAML header.
- Notes are written once. An entry changes by a new entry that names it in
  `supersedes`. What is current, an entry's status and its count of flags are
  derived when read and never stored, so no file is ever edited.
- Every write is one git commit.
- The service loads the library into memory at start, serializes writes, and
  searches in-process (word stems ranked with BM25).
- The HTTP API and the `cmem` CLI stay as they were (ADR 0012).

## Consequences

- One service instance per library. That is enough for one owner's workers and
  laptop; it would not be for many concurrent writers.
- A person with the files can change or delete a note, which the service never
  does. That is not prevented. `cmem check` reports it from git history, and
  it should run in CI on the library's remote.
- Notes added by hand are picked up when the service next starts.
- Operational logs (what was shown to which session) live in `.cmem/` inside
  the library, outside git, and can be thrown away.
- Embeddings, if ever needed, would be a rebuildable index beside the files.
- The Postgres code, its migrations and the `pgx` dependency are removed.
