# 0026: `--here` does not change who a replacement is for

Status: Proposed

## Context

ADR 0021 settled that a replacement which names no scope keeps the scope of
the entry it replaces, and that making an entry apply everywhere has to be
said (`--global`). It was written against the costly mistake, a rule given
for one repository being followed in all of them, and said nothing of the
other direction.

Since ADR 0023 a new note has to say who it is for, and the session-start
text teaches `--here` or `--global` as part of every `cmem note`. Agents now
pass one of them with `--replaces` too. On the transfer benchmark, in the
case where a rule about the machine's kubectl context changes, 4 of 4 agents
recorded the rule with `--global` and a later session replaced it with
`--here --replaces ID`. The corrected rule was then shown in one repository
only, and the probe in the other repository failed 4 of 4.

## Decision

On `cmem note --replaces`, `--here` does not set the new entry's scope. The
replacement is for whoever the old entry was for, as when no scope is given.
When that differs from the repository the session is in, the answer says so
and names the way to narrow it on purpose: `--scope`, which a replacement
has always honoured.

`--global` on a replacement still widens, as ADR 0021 decided. Widening is
the direction that needs saying, and an agent that says it is answering the
question the session text asks.

With that in place the same case was run four more times on the same model.
All four agents again passed `--here` with `--replaces`; all four entries
still applied everywhere, and the probe in the other repository passed 4 of
4.

## Consequences

- A rule that applied everywhere keeps applying everywhere when it is
  corrected, whatever the agent's habit with `--here`.
- An agent that means to narrow an entry ("that only holds in this
  repository") has to read the answer and run the command again with
  `--scope`. That is one more step in a rare case.
- The same reflex in the widening direction is still possible: an agent
  that replaces a repository's entry and passes `--global` out of habit
  widens it. The benchmark has not shown that happening.

## Alternatives considered

- **Hold the note and ask.** Costs a turn every time, for a question whose
  answer in every run seen was "I did not mean to change it".
- **Refuse `--here` with `--replaces`.** Simpler to explain, but a command
  that worked is then an error, and the agent has to guess why.
