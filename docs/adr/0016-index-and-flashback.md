# 0016. Memory reaches the agent unasked: an index at session start and flashback at key moments

Status: Accepted (2026-11-19, as proven in use; proposed 2026-11-18). Built; whether it helps is for the benchmark to
say. The owner's stated concern is context bloat.

## Context

A memory the model must remember to query is weaker than Claude Code's
built-in memory, whose index is in every session. And a real case showed a
moment no query would have covered: an agent recommended hard topology spread
constraints, and only its user remembered that the practice had already caused
trouble. The agent had even seen evidence against it earlier and did not
connect the two. The user's question shared almost no words with the lesson.
The agent's own recommendation shared many.

## Decision

- **Index.** At session start the harness adds `cmem index`: the current
  entries for the session's scope, trusted and newest first, within a
  character budget (2,000 by default), plus one paragraph on using `cmem`.
- **Flashback.** At key moments the harness passes the moment's text to
  `cmem flashback`. It returns at most three entries, only those that overlap
  the text closely, and never one the session has already been shown. When
  nothing is close it returns nothing and the context is untouched.
- **Moments, in Claude Code:** a new prompt, a shell command about to run, a
  failed tool call, and the agent's own reply before the turn ends. Before a
  command, an entry that names a program the command runs counts as close,
  because a command such as `kubectl get nodes` shares only that one word with
  the lesson about it. On the last, if memory has something
  close, the turn continues once so the agent can revise.
- **No model.** Closeness is word overlap: at least two shared word stems,
  covering at least 40% of the entry or of the text. A small model as a
  relevance filter is added only if measurement shows it is needed.
- **Corrections are captured.** The session-start text tells the agent to note
  what the user corrects it on.

## What the retrieval set says so far

Measured on `bench/retrieval/set.json` (24 entries, 26 moments, written by the
same hand as the code, so treat as optimistic):

| Threshold | Precision | Recall | Quiet moments kept quiet | Characters added per moment |
|---|---|---|---|---|
| 0.3 | 92% | 79% | 92% | 43 |
| 0.4 (default) | 100% | 64% | 100% | 33 |
| 0.5 | 100% | 43% | 100% | 21 |

The cost in context is small because most moments add nothing. The misses at
the default are the cases where the moment and the lesson use different words
("run the tests" against a note about the TZ variable). Those need either
better-worded entries or a semantic step.

## Consequences

- The reply-time trigger costs one extra model turn when it fires.
- A session resumed after compaction gets the index again, but not flashbacks
  it was already shown.
- Each harness needs its own small adapter for its hook mechanism. The portable
  part is the two CLI commands; a harness without hooks falls back to the
  model calling `cmem recall`.
- The service logs what it showed to which session, which later tells the
  dreamer which entries get surfaced and which get flagged afterwards.
- The task-stream benchmark needs an arm with the hook and one without, with
  tokens added by memory reported, to answer the context-bloat question on
  real tasks.
