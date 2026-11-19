# 0021. A replacement keeps the scope of what it replaces; applying everywhere is said out loud

Status: Accepted (2026-11-19). Built.

## Context

An entry's scope decides who sees it: sessions in the repositories it names,
or every session when it has none. Importing Claude Code's memories brings in
entries scoped to a repository for the first time; until now every entry in
use had no scope.

Two operations create an entry in place of others: a working agent's
`cmem note --replaces`, and the consolidation role's `cmem supersede`. Both
took the new entry's scope from the command alone, so leaving `--scope` out
turned a repository's entry into one shown everywhere. The costly mistake with
scope is exactly that one: a rule given for one repository, such as "the agent
may commit here without asking", being followed in all of them.

## Decision

- A replacement or a merge that names no scope keeps the scope of the entries
  it replaces.
- A merge of entries whose scopes differ is refused until the caller says
  what the result's scope is.
- Making an entry apply everywhere is explicit: `--global` on `cmem note
  --replaces` and on `cmem supersede`. Giving both `--global` and `--scope`
  is an error.
- The consolidation routine may merge the same lesson found in several
  repositories into one global entry when it is about the person or their
  machine, into one entry scoped to those repositories when it is about them,
  and may retire a scoped entry that only repeats a global one. It never
  widens a single scoped entry on its own, never narrows, and never moves an
  entry between repositories; those it lists for the owner.

## Consequences

- Promoting a lesson to every repository takes a person saying so in a
  session, or the same lesson turning up in more than one repository.
- Notes written by working agents still have no scope unless the agent adds
  one, and the session text does not tell agents to. So everything an agent
  learns is global today. That is what carried lessons across repositories in
  the transfer benchmark; it will also put one repository's facts in front of
  every session. Whether and how agents should scope what they note is open.
