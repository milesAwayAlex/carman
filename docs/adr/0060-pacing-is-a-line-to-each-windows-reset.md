# 0060: Pacing is a line to each window's reset, with the rate it is used at, and room that will be lost is said

Status: Proposed (2026-11-27). The owner, for one night: "use the most up
to that point [the five-hour reset], then the most on the next window ...
worst case use everything up to 95%". Adds to ADR 0043 and ADR 0052.

## Context

The plan's usage windows (five hours, seven days, and some for one model)
each have a utilization and a reset, read hourly for every model in use
(ADR 0043, 0052). What is not used by a window's reset is lost. The
admission rule built so far (ADR 0043, off) is a threshold: below a
priority, work is held while a window is above a level. It does nothing
until the level is reached, then holds everything below the priority
however close the reset is, and it says nothing of a window that will
reset half unused while work waits. R7 asks for both sides: backlog work
stops before the headroom is used, and unused capacity at reset stays
under a target.

## Decision

**The line.** A window's start is its reset less its length (five hours;
seven days for every `seven_day*` window; a window of another name is not
paced). Its line runs from zero at the start to the target at the reset
(`dispatcher.pace.target`, 0.95). A window is ahead by how far its latest
reading's utilization is above the line now, and under it otherwise.

**The rate.** The readings of one window are those whose reset is the
latest one's (within half an hour): the shared windows from every model's
readings, a model's own windows from its own. The window's start, at
zero, counts as a reading. The rate is the slope from the earliest
reading within a span back from the latest (two hours for a five-hour
window, a day for a seven-day one; the latest before the span when none
is inside it) to the latest. So:

- with few readings, or one, the rate is the average since the window
  began, which is a real reading of the same quantity, not a guess;
- across a reset, the old window's readings are not used; a window whose
  reset has passed since its latest reading is a new window at zero, and
  admits;
- after an account change (`model.account`), the readings from before are
  left out, as status already does (ADR 0055); the new account's window
  still began at zero at its start, so its first reading gives its rate.

From the rate: the utilization expected at the reset, when the target is
reached if before it, and the room under the target expected to be left.

**Admission** (`dispatcher.admission.mode: curve`). Work at or above
`below` always runs. Below it, a task runs when each window it draws on
(the shared ones and its first model's own) is on or under its line. A
window ahead of its line holds it, with the reason and the time it is
expected to be admitted: when the line reaches the window's present
utilization (if nothing more were used), or `pace.soon` (1h) before the
reset, whichever is first. A window that resets within `soon` with
utilization under the target admits everything. A reading older than
`maxAge` is unknown and admits; a model that is refused holds what needs
it first, as in ADR 0052.

**Room that will be lost.** With `dispatcher.pace.room`, after each new
reading the dispatcher records `usage.room` (subject the window, with
`/MODEL` for a model's own) for each window expected to leave at least
`pace.roomMin` (0.1) under the target at the present rate: the
utilization, the line, the target, the rate per hour and since when, the
reset, the expected utilization at it and the room. Its ID is the
reading's and the window's, so a restart does not repeat it. It is for
whatever runs deferred work to act on; this change builds no queue.

**Shown.** `dispatch status` gives each window a line: utilization, line,
target and reset, then "AHEAD by N points; at R%/h it reaches 95% at T,
D before its reset", or "under by N points; at R%/h it ends near U%, M
points UNUSED". It paces against `DISPATCHER_PACE_TARGET` in the client's
environment, or 0.95.

**The threshold stays, beside the curve.** `mode` is `threshold` unless
set, so an installation that switched admission on keeps what it has,
with `above` as it was. The curve does not need `above`: its line never
exceeds the target, so it already stops backlog work below it. Both modes
share `below`, `maxAge` and the refused-model check.

## What this does not do

- No queue of deferred work, and nothing acts on `usage.room` yet.
- No tiers between "always" and "paced": one priority line, as before.
- The rate is the account's, all agents together; work held does not
  lower the estimate until the next reading shows it.
- The top-level binding limit of a reading with no window beside it is
  not paced (its length is not known).

## Consequences

- Utilization is read hourly and in whole points, so a seven-day window's
  rate over a few readings is coarse; a day of readings smooths it.
- A five-hour window ahead early in its life holds backlog work for an
  hour or more; one that resets soon with room admits it all, which is
  the owner's "use the most up to that point".
- `usage.room` adds at most one event per paced window per reading.
