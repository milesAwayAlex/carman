# new-a-message-for-a-conversation-that-is-not-there: A message for a session whose conversation is not there is read in a new conversation, told so

Status: Proposed (2026-11-28).

## Context

A message for a session that is not running becomes a task that resumes
its conversation as its agent (ADR 0045). `agent-resume --message` looked
for the conversation's file and, finding none, ended with status 1: the
job failed, a failure notice was made, and the message was never read.

A job's conversation is kept only where the installation gives it a place
(`agent.sessionStorage`, empty by default). Without one, every answer to a
run that a message started met this: seen in a trial, where an agent wrote
to another, the other answered, and the run started for the answer failed
with "asker has no conversation ...". An installation that keeps
conversations does not show it, unless one is lost.

## Decision

Two things could be done with such a message.

- **Refuse it at the dispatcher**, with word to the sender that the
  session it answers cannot be resumed. The sender then knows, and nothing
  runs with less than it should know. But the dispatcher does not know
  what a pod's volume holds, so it would refuse by the installation's
  setting alone, and on such an installation no agent's run could ever be
  answered: a sender would have to write to the agent by name and say
  everything again.
- **Read it in a new conversation under the session's id.** Taken. The
  message reaches the agent it was for, the session keeps its id (answers
  and watches still name it), and the run is told first that the earlier
  conversation is not there, why, that the message is all there is to go
  on, and that where the message alone does not say what to do (a yes to
  something it cannot see) it must not guess or act but ask the sender to
  say it in full.

The two cases are told apart by whether the pod has the place where
conversations are kept:

- **the installation keeps none**: the run is told so; a line of the
  job's log says it. Nothing was lost;
- **the installation keeps them and this one was not found**: something
  was lost. The run is told so, and its answer ends with a line saying it
  was written without the conversation the message was for, so that the
  sender, and the owner reading the exchange, can tell.

## Consequences

- An answer to a job's run is read on every installation. Checked by
  `scripts/check-trial-messages.sh`, in a trial, which keeps none.
- A run with nothing to go on costs a run and may only ask the sender to
  say more: one more exchange, bounded like any other (six answers).
- A lost conversation no longer fails its job and makes no failure
  notice. What says so is the line in the answer and the job's log; no
  event of its own is recorded. If losses should be counted, that is an
  event to add (`message.unresumed`), not built here.
- Not tried: the lost case on an installation that keeps conversations
  (the words differ by one test of a directory), and what a real model
  does with the instruction not to guess.
