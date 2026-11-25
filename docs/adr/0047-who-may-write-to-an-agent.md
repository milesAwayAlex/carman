# 0047: Who may write to an agent: the owner, its own sessions, an answer, and the agents it lists

Status: Proposed (2026-11-25). Built and tested against the dispatcher's
database; not yet deployed.

## Context

Under ADR 0045 any agent whose pod the cluster vouches for may write to any
other agent by name, which starts a run of that agent with that agent's
credentials, and to any of its sessions, running or kept. The receiving
session is told that a peer's message is information and not the owner's
instruction, and that a message does not widen its limits. Nothing else
stands between an agent that is confused, or misled by what it read, and a
run of an agent with more reach than its own: one that can open pull
requests, say, or write to a cloud account.

The owner asked for a limit: each agent's definition says who may write to
it; with nothing said, only the owner may.

## Decision

A message is sent when one of these holds, and refused otherwise:

1. The sender is the owner.
2. It is for the owner. Any agent may write to the owner.
3. The sender is the same agent as the addressee: sessions of one agent
   write to each other, and to their agent by name.
4. It answers a message that was sent to the sender (to its session, to
   another session of its agent, or to its agent by name) and goes back to
   whoever sent that message. Whoever writes to a session may be answered.
5. The sender's agent is in the addressee's `writtenBy`: a list of agent
   names in that agent's entry under `agents` in the values. It covers the
   agent by name and all its sessions.

With no `writtenBy`, only 1 to 4 apply. **This changes ADR 0045's
behaviour**: an agent that could write to another agent before cannot,
until the other agent's definition lists it.

The rule is about agents, not sessions: which session of its pod a caller
is remains its word (ADR 0045), so a list of sessions would check nothing.
It is one way: `writtenBy` on B naming A lets A write to B, not B to A,
except by answering.

A refused message is refused at sending, as an address nobody has is:
`dispatcher send` says `refused` and exits 4, and the reason names who may
write to that agent (the owner, its own sessions, and the agents listed),
that the sender may answer a message that agent sends it, and that the
owner adds the sender to `writtenBy` to allow it. The names of agents are
not a secret from an agent: `peers` lists them all. It is recorded as
`message.refused`, with the address it resolved to.

`dispatcher peers` still lists every agent and session, and marks the ones
the caller may not write to "not writable by you", with who may. The owner
sees everything writable.

The chart hands the lists to the dispatcher as it hands it the schedules:
`DISPATCHER_WRITERS`, a JSON object from each agent to its `writtenBy`,
made from the values. A name in a `writtenBy` that is not an agent of the
values fails the render. The dreamer is not an agent of the values and
cannot message (ADR 0045), so it cannot be named.

An agent that is not in the values (one made by hand with kubectl, as the
trial of ADR 0045 was) has no list, and so only 1 to 4.

`dispatcher watch` is not limited: it tells the watcher when a session goes
idle and writes nothing to the session.

## Consequences

- An agent meant to hand work to another must be listed in that agent's
  `writtenBy`. Until someone lists it, each such message is refused with
  the reason, and the owner hears of it only if the sender tells them.
- Two agents that answer each other still stop at ADR 0045's six answers.
- A run started for a message to an agent by name answers through the same
  rule (4), so it can always send its result back.
- The lists live in the values, beside the rest of an agent's definition;
  changing them takes a deploy, as a schedule does.

## What would make us look again

- Wanting a finer grain than the agent: a session, or a kind of message
  (an answer, a question, a request for work).
- Wanting an agent to be allowed to write to another only through the
  owner's approval of each message.
