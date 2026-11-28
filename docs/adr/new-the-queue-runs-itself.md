# new-the-queue-runs-itself: The owner's list is worked through by itself

Status: Proposed (2026-11-28).

## Context

Every piece of work started because somebody sent a message: nothing took
the next task when one finished. The owner has a list of what is to be
done, in plain words, under his priorities (ADR 0077: `dispatcher priority
add`), which until now only said what waits.

## Decision

**The dispatcher starts the next item when there is room.** With
`dispatcher.queue.enabled`, while fewer than `running` (1) of the queue's
items run, it takes the next item that waits (priorities by share, the
largest first; items as the list orders them) and sends its words, as the
owner, to the agent the installation names (`queue.agent`, one that takes
messages). The item is marked started, by the queue, and the message it
became is recorded (`queue.started`).

**An item ends with its run.** The run's answer goes to the owner like
any answer to him, so he is told it with its first line; the item is done
with that first line as its note. A run that failed, stopped at the
plan's limit, ran out of time or could not be sent marks the item
**failed** (a state the list gains), saying which. A failed item is not
taken again by itself: `dispatcher priority again ID` puts it back to
wait. The queue goes on either way. An item somebody began by hand is not
the queue's to end.

**The model.** An item runs on the model it names (`--model`), on
`queue.strongest` when it is marked `--hard`, and otherwise on
`queue.model`, which the chart refuses to be the strongest. The run is
told its model by the dispatcher (`TASK_MODEL`); a sender cannot say one.

**Usage.** Before an item starts, ADR 0060's rule is asked for its model,
whether or not admission is on for tasks: an item whose model is refused
waits until that model's reset, and one whose windows (the shared ones,
and its model's own) are ahead of their line to the reset waits for the
line. An item that is held does not hold the ones behind it, so when the
strongest model is spent the queue goes on with what does not need it.
The owner's own messages are not items and are never asked this.

**He sees it.** `dispatch priority list` shows, under each priority, what
runs, what waits (with why it is held, as of now), what failed and how,
and the five latest done, each with the first line of its result.

Not chosen: a second list beside the priorities (one list, one place to
look); retrying a failed item (a run that failed for its task would fail
again, on the plan's usage); a planner that orders or sizes items (the
order is the owner's).

## Consequences

- With one priority it is a plain queue. With several, the larger share
  goes first and a smaller one runs only when nothing of a larger waits
  unheld: shares do not yet divide the queue's runs by usage (ADR 0077
  says nothing measures what a priority used).
- An item is sent as the owner, vouched for by the dispatcher: the agent
  treats it as his request. Whoever may add to the list (the owner and
  his machine's sessions) may so start work on that agent.
- If the dispatcher stops between marking an item started and sending it,
  the item is failed two minutes later, saying so.
- Tried in a trial with a real harness and the stand-in
  (`scripts/check-queue.sh`): three items, the second hard, the strongest
  model refused before the start. The first and third ran by themselves on
  the plain model and were done with their first lines; the hard one
  waited with its reason shown, and ran on the strongest when that
  answered again; an item whose run was killed was failed and not taken
  again.
- Not tried: a real model doing an item unattended; two items at once
  (`running: 2`, in the Go test only); the line to the weekly reset
  holding the queue on real readings (the Go test, and by accident the
  trial's first run, where a week at 100% held everything); a run that
  times out (the Go test's "ended with no answer").
