# new-a-message-at-a-time: A sender asks the dispatcher for a message at a time, which is kept as an event and sent then as an ordinary message

Status: Proposed (2026-11-28). Adds to ADR 0045.

## Context

A session that directs others said "I send the next task at 15:40 UTC",
ended its turn, and was idle at 15:40: nothing wakes an idle session but a
message or a person, and nobody sent one. The session was not wrong to
promise; it had no way to keep the promise. The harness has no timer that
outlasts a turn, a `sleep` in a shell holds the turn open and ends with
the pod, and a schedule in the installation's definition (ADR 0043) is the
owner's to write, not a session's, and starts a job, not a turn of this
conversation.

What wakes a session already exists: a message. What is missing is a
message that is sent later.

## Decision

**The command.** `dispatcher send --at TIME TO 'TEXT'`. `TO` is any
address `send` takes, the sender's own included (`AGENT/NAME` or
`session/ID`). `TIME` is one of:

- a clock time in UTC, `15:40`: the next time it is that, today or
  tomorrow; asked within that very minute, it is now;
- a date and time in UTC, `2026-01-02T15:04Z`, with or without seconds
  and the `Z` (any RFC 3339 time is read, with its own zone);
- how long from now, `90m`, `1h30m`.

Times are UTC because the dispatcher's log, its messages and the sessions'
own words about time are; a session in a pod has no zone of a person's.
The dispatcher reads the time with its own clock, so a pod's clock does
not matter. The answer says the time it understood and how long that is
from now, which is where a misread shows. A time more than a minute past,
or more than seven days ahead, is refused. `dispatcher timed` lists the
sender's messages that wait for their time (the owner's lists everyone's),
and `dispatcher timed cancel ID` cancels one: its sender may, and the
owner. `dispatcher message ID` shows one before and after.

A flag on `send` was chosen over a command of its own because what is
asked for is a message, written and addressed as one, and a session
already knows `send`. An answer (`reply`) is not taken with `--at`: an
answer is to a message that waits for it now.

**Kept as an event.** What was asked is `message.timed` in the
dispatcher's log (who asked, as the dispatcher knew them then; to whom, as
written and as resolved; the words; when asked; for when). What became of
it is `message.timed.done`, claimed once: sent, cancelled or refused. The
log is the dispatcher's database, so a restart loses nothing and the
nightly backup holds it (ADR 0038); they are kept as long as messages are.
Nothing is held in memory but which are done.

**Sent as an ordinary message, at the time.** The router's pass (every
three seconds) sends each message whose time has come through the same
path as `send`, as the sender: the address is resolved then, and who may
write to whom (ADR 0047) is judged then, so a right that was taken away
between the asking and the time is not used, and one given is. The rules
are also looked at when it is asked for, only so that a mistake is said at
once. From there it is a message like any other: it wakes an idle
session, waits for one that is not running or starts it, counts against
the sender's runs, and may be answered. Its words begin with one line
saying that it was asked for at one time for another, so that a reader,
the sender itself included, knows it is not written now.

It is sent under the id it was asked for under. A dispatcher that stopped
after sending it and before recording that finds the message there and
does not send it twice; one that was down at the time sends it when it is
back, and the line says how late. Sending and cancelling one message are
kept apart in the one dispatcher there is: a message is marked as being
sent, under a lock that is not held while it is sent (sending may start a
run), and one being sent is no longer cancelled.

**When it cannot be sent at the time** (the addressee has gone, the
sender may no longer write to it) that is recorded as `refused`, with the
reason, on the `message.timed.done`, and the sender is told by a message
from the dispatcher, with the reason and how the message began. Where the
ordinary path records a refusal of its own (`message.refused`: no such
addressee, or not allowed to write to it) that is there too, under the
same id; a refusal for another cause is on the `message.timed.done` only.

**Limits.** Twenty waiting for one proved sender: an agent, all its
sessions together (a pod proves which agent it is, not which session),
the owner's machine, or the owner; seven days ahead. A pod asks as a
session the dispatcher has heard from, or as its agent, not as a name it
makes up. A session
may ask for a message to itself each time it is woken, which is a
schedule of its own making: each such turn is the session's choice and
costs what a turn costs. It is not prevented; `timed` shows it to the
owner, who can cancel.

**Sessions are told.** The words a session is given at its start
(`SessionStart`) say: when you say you will do something at a time, ask
for the message that wakes you in the same turn, and nothing else wakes an
idle session at a time.

Candidates not chosen:

- *A task on a schedule* (ADR 0043's machinery, one-off). It would start
  a job for the session; a message already does that when it must, and
  wakes the open session when it need not.
- *The harness's own scheduled prompts.* They live in the harness's
  process: gone with it, unknown to the dispatcher, not in the backup,
  and not there for another harness (M8).
- *A timer in the pod* (`sleep` and `send` in the background). It ends
  with the pod and nobody can list or cancel it.

## Consequences

- A promise at a time is kept by the dispatcher, whether the session is
  idle, busy or not running then, and whoever reads the log sees what was
  promised and what became of it.
- A message may arrive that its sender no longer means. It can be
  cancelled; the first line tells the reader when it was written.
- The time is as exact as the router's pass: within about three seconds.
- The owner can use it too (`scripts/deploy.sh CONTEXT dispatch send --at
  ...`): a note to a session, or to himself, at a time.

## The checks

Tests named for R2 (`internal/dispatch/pgxdispatch/timed_test.go`,
`internal/dispatch/message/timed_test.go`): sent once at its time and not
before, by a dispatcher started in between; not twice when the stop came
after sending; cancelled by its sender or the owner and by no other; the
right to write judged at the time and the sender told of a refusal; the
limits; the door and the router's pass. `scripts/check-timed.sh CONTEXT`
tries it in a trial with a real harness and no model: a session asks for a
message to itself, the trial's dispatcher is restarted, and at the time
the idle session is woken by it.
