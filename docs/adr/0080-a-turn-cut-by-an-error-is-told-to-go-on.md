# 0080: A session whose turn ends on an error of the model's API is known to be idle, listens again, and is told by the dispatcher to go on

Status: Proposed (2026-11-28). Adds to ADR 0045 (how a session is known to
be busy or idle) and ADR 0075 (a session that does not hear is noticed).

## Context

A deploy restarted the egress proxy while a standing session was in a
turn. The turn ended with an API error and the session sat at an empty
prompt for 75 minutes. The dispatcher had it as busy all that time, so a
message for it waited "until its next tool call", and the pod's watcher,
which gives a turn to a session that is idle with no listener, did
nothing. A person typed in its window and it went on.

What the harness does was tried (2.1.326, in a trial, with the model's
stand-in dropping the connection in the middle of its answers):

- An answer cut once, or the proxy's pod deleted with an answer under
  way, ends nothing: the harness tries the request again (ten tries with
  growing waits, then whole answers in place of a stream, about three and
  a half minutes in all) and the turn goes on.
- When every try fails, the turn ends with "API Error" on the screen and
  the window is at an empty prompt. **No Stop hook runs**, so nothing
  says the session is idle and no listener is started: the state seen on
  the night.
- **The harness's `StopFailure` hook runs then**, once, with the kind of
  error.
- While a turn runs, the line the cursor is on is the same empty prompt
  as when idle; only the line above it moves. A rule on the screen (busy,
  and nothing changed for some minutes) could tell a stalled session, but
  it would have to guess what the hook says outright.

## Decision

- **The `StopFailure` hook is the dispatcher's, as `Stop` is.** It says
  the session is idle and that its turn was cut, with the error; and it
  starts the listener, as the end of any turn does. So a message that
  waited is taken at once, and one sent later wakes the session.
- **A cut is recorded**: `session.cut` (the session, the error, how many
  in a row).
- **The dispatcher tells the session to go on.** A minute after the cut
  it sends the session a message of its own: the last turn was cut by an
  error, nobody stopped it, go on, and look at what a half-done step left
  before doing it again. The message wakes the session as any other. If
  the turn it starts is cut too, the next comes after two minutes, then
  four, eight and sixteen: an error that lasts is not hammered. After
  five cuts in a row nothing more is sent, which is recorded; a turn that
  ends well starts the count anew. Only interactive sessions are told: a
  run that ends on an error has failed, and its task says so.
- **Nothing is typed into a window for this.** The watcher's line (ADR
  0075) is for a session with no listener; a cut session has one again.
  If the listener did not start, the session is idle with no listener and
  a message waiting, and the watcher's rule applies as it is.

Candidate not chosen: *the watcher types a line when a busy session's
screen has not changed for five minutes.* It needs a guess where the
harness gives a fact, waits five minutes where one will do, and types
into windows. It would cover a harness that hangs without ending its
turn, which has not been seen; if it is, this is the rule to add.

## Consequences

- A session cut by a restart of the proxy goes on by itself after about
  a minute, at the cost of one turn it did not ask for.
- A session cut in the middle of a step may do part of it twice; the
  message says to look first.
- An outage of an hour costs each open session five short turns that
  fail, then silence until a person or a message comes.
- A harness without the `StopFailure` hook behaves as before this.

## The checks

`internal/dispatch/pgxdispatch` (`TestR2CutTurnIsToldToGoOn`): a cut is
idle and recorded; told after a minute, once; twice as long after the
next; anew after a turn that ended well; five times and no more.
`scripts/check-cut.sh CONTEXT`: a trial with a real harness and the
stand-in cutting twenty answers (`stand-in: cut N`): the harness gives
up on the turn, the hook runs, the session is recorded as cut, idle and
listening; a minute later it is told to go on and the turn that follows
ends well; a later message is delivered as before.
