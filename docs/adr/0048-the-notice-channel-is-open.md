# 0048: The channel that carries notices to the owner is mostly trusted, not private

Status: Accepted (2026-11-26). The owner: "it's not entirely private, but
it's still mostly trusted - so it's fine to tell me what the message is
about, but no PII or any internal identifiers/keys. i still want to know
roughly what happened and how urgent it is." (A first version of this
record, written the same day from his shorter instruction to treat the
channel as open, kept everything out of a notice; he corrected that.)

## Context

An agent gets the owner's attention with the installation's `notify-owner`
command (ADR 0036), and the dispatcher sends notices of its own through the
same command: a job that failed, a run that did not happen, a message
addressed to the owner (ADRs 0043 and 0045). On the installation this is
built on, the command sends a chat message through a company's workspace.

A chat workspace is not the owner's alone: its operator, its administrators
and its integrations can read it, and it keeps what it was sent. It is
still where the owner looks first, and a notice that says nothing has to be
opened to learn whether it matters.

## Decision

- **A notice says roughly what happened and how urgent it is**, so that the
  owner knows what it is about without opening anything.
- **It carries no personal data and no internal identifiers or keys**: no
  person's name or address, nothing about a customer, no account or project
  ids, hostnames, tokens. Those stay where only the owner reads them: the
  pull request, the ticket comment, the document, the dispatcher's log.
- **A message to the owner travels as its first line.** The sender writes
  that line as the summary; the notice carries it, names the sender, and
  gives the command that reads the whole message and the one that answers
  it. The rest of the message stays in the dispatcher's log.
- **Agents are told so** in the words every routine gets about the owner's
  attention, and an installation's own `notify-owner` says it in its help.

## Consequences

- What is personal or an identifier is the agent's judgement, for what it
  writes itself; nothing checks it. The dispatcher's own notices of
  failures and of what did not happen name the job and the time, which
  this installation does not treat as identifiers.
- A message whose first line is not a summary sends that line all the
  same; a sender that puts an identifier there has sent it.
- It narrows the part of ADR 0036 that has a notice explain the matter in a
  few sentences: it still does, within these limits.
