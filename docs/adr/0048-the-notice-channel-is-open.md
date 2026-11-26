# 0048: The channel that carries notices to the owner is treated as open

Status: Accepted (2026-11-26). The owner's instruction: "please treat slack
as an 'open' channel, no sensitive information should cross it."

## Context

An agent gets the owner's attention with the installation's `notify-owner`
command (ADR 0036), and the dispatcher sends notices of its own through the
same command: a job that failed, a run that did not happen, a message
addressed to the owner (ADRs 0043 and 0045). On the installation this is
built on, the command sends a chat message through a company's workspace.
What it carried was whatever the agent wrote in a few sentences, and, for a
message to the owner, the message itself.

A chat workspace is not the owner's alone: its operator, its administrators
and its integrations can read it, and it keeps what it was sent. Agents work
with material that should not go there: findings about systems, costs,
names.

## Decision

- **A notice says that there is something for the owner, what kind of thing
  it is, how urgent and how important, and where to read it. It does not say
  what the thing says.** No finding, figure, name of a customer or a person,
  or detail of a system goes in a notice's title or text.
- **A message to the owner is not carried in its notice.** The notice names
  the sender and gives the command that reads the message and the command
  that answers it. The message stays in the dispatcher's log.
- **Agents are told so** in the words every routine gets about the owner's
  attention, and an installation's own `notify-owner` says it in its help.
- **Where to read it is somewhere only the owner reads**: the link to the
  pull request, the ticket comment or the document; the dispatcher's
  commands for a message.

## Consequences

- The owner has to go and look: a notice no longer tells them the decision
  at a glance. That is the cost, and it is the point.
- The rule rests on what agents are told, for what they write themselves;
  only the dispatcher's own notices are made so by construction. An agent
  can still put a finding in a title.
- The dispatcher's notices of failures and of what did not happen name the
  job and the time, which this installation does not treat as sensitive.
- It supersedes the part of ADR 0036 that has a notice explain the matter
  in a few sentences.
