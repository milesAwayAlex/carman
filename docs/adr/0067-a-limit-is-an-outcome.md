# ADR 0067: A run that stops at the plan's usage limit says so to whoever waits on it

Status: Proposed (2026-11-27). Adds to ADR 0045 (what a run started for a
message sends back) and ADR 0053 (what a standing agent's pod attends to).

## Context

Seen twice in one night. A run started for a message hit the plan's limit,
and its answer to the sender was the harness's one line ("You've hit your
5-hour limit · resets 10:40am (UTC)"), which reads like an answer unless
one reads it. And a standing session stopped mid-task for the limit; the
session waiting on it heard nothing until it went on. The harness of a
standing session waits for the reset by itself and says nothing: no hook
of the turn's end runs. It writes the stop into its transcript as its own
entry for an error from the model (an assistant entry of model
`<synthetic>`, `isApiErrorMessage`), which the pod's watcher can read.

## Decision

- **A run for a message that stops at the limit is an outcome.**
  `agent-resume --message` answers with `dispatcher answer --failed` when
  the harness failed or called its result an error. The dispatcher, given a
  failed run's words that are the plan's limit, records
  `message.limited` (the words, when it resets, when it is sent again) and
  sends the sender, as the answer, that the work was not done, when the
  limit resets, and whether it will be sent again. Words alone, from a run
  that finished, are an answer as before: a session may quote them.
- **Sent again once, by the dispatcher**, a few minutes after the reset (an
  hour on, when the reset cannot be read), when the sender asked for that
  (`dispatcher send --again-after-limit`) or the installation says so for
  every message (`dispatcher.messages.againAfterLimit`). The message sent
  again is the same, from the same sender, with `resends` naming the first;
  `message.resent` records it. One sent again is not sent a third time.
- **A standing session stopped at the limit** (ADR 0053): `dispatcher
  attend` reads the last entry of each running session's transcript, and
  on such a stop tells the dispatcher (`/v1/attend/limited`), which tells,
  once for each stop, the senders of the messages the session took in the
  last day and has not answered, and whoever watches it for its next idle
  (who are still told then). `session.limited` records it.
- The limit's words are read as the routine runner reads them (`resets
  10:40am (UTC)`, and also `resets Dec 1, 5pm (UTC)`), with a number in the
  limit's name ("5-hour") now matched there too.

## Consequences

- The run's Job still fails, and the owner hears of a failure as before.
- Sending again uses the sender's run limit (ADR 0045) as any message does.
- What the stop's entry looks like was read from the harness's transcripts
  for other errors (Claude Code 2.1.326); a stop for the limit was tried
  with a stand-in for its words, not a real limit.
- A claim taken and then a crash before the message is sent again loses
  the second sending; the sender was told it would come.
