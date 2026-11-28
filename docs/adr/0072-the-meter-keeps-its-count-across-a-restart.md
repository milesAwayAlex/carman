# 0072: The meter sends what it holds when told to end, writes a long session's use in parts, and takes the trials' day back from the log

Status: Proposed (2026-11-28). Changes how ADR 0050's tallies close and
ADR 0065's count of trials' requests.

## Context

The meter beside the egress proxy (ADR 0050) adds up each pod's use in
memory and writes it to the dispatcher's log as `model.usage` when the pod
has been quiet for `METER_IDLE` (2m) or the tally is `METER_LONGEST` old
(15m). A deploy restarts the meter. In a trial with a meter of its own, a
redeploy lost the tally of a pod still running: its run showed as one the
proxy saw nothing of.

The meter did try to send what it held at its end, but at the wrong
moment. Told to end, it started its last send at once, beside the
shutdown of its listener; an answer still streaming was counted after
that send, into a ledger nobody read again. And the last send had ten
seconds for one try: a deploy that restarts the dispatcher too (a changed
image restarts both) refused it, and the records were then in the meter's
own log only.

Two other counts are in the meter's memory. The budget at the proxy (ADR
0064) already takes the day back from the log at the meter's start
(`/v1/meter/standing` with `meter` and `since`); it was short only by what
the lost tallies held. The count of trials' requests a day (ADR 0065) was
the process's own: a restart gave trials their day again.

## Decision

- **Told to end, the meter lets its answers finish, then sends.** On the
  signal a pod gets at its termination, the meter stops taking requests,
  waits up to `METER_DRAIN` (10s) for the answers under way (each is
  counted when it ends, whole or cut), and only then closes every tally
  and sends it, trying again every two seconds for up to `METER_FINAL`
  (20s), while the dispatcher may be restarting. What is not sent by then
  is a line of its log, as before. The proxy's pod has a grace period of
  40 seconds for this (`terminationGracePeriodSeconds`, with the meter).
- **A long session's use is written in parts, every five minutes.**
  `egress.model.longest` is 5m, not 15m: a meter killed outright (no
  signal: a lost node, the memory limit) loses at most five minutes of
  each pod's use, not a run. A part is a record like any other: its event
  ID is the meter, the agent, the pod, the model and the time the part
  began, so each part is its own event and a part sent again is the same
  one; `dispatch status` and the standing add records up, so a pod's use
  in parts sums to the whole. The cost is three times as many
  `model.usage` events from a busy pod (twelve an hour while it asks).
- **The trials' day is taken back from the log.** The meter asks the
  dispatcher (`GET /v1/meter/trials`, the meter role, with `meter` and
  `since` as for the standing) at once and every minute for the records
  of trials' agents (`trial/...`) of a model that ended within the day,
  less its own since it started, and counts them with what it passed
  itself. A record's requests count from the record's end, so they free
  up at most a tally's length late.

## Consequences

- A deploy no longer loses what a pod used, unless the dispatcher is away
  for longer than the meter's last twenty seconds.
- The trials' count from the log is of answers that said what they used:
  a request admitted whose answer was an error is not in it (it is in a
  record with no model, beside refusals). It errs toward letting a few
  more pass, and those cost nothing.
- Until the first answer from the dispatcher, a few milliseconds after a
  start, a new meter holds nobody to a budget and counts only its own
  trials' requests, as before.
- With a rolling update, the old meter's last records reach the log after
  the new one has started; the new one counts them at its next question
  (half a minute for the budget, a minute for trials).
- Readings of the account's standing still waiting to be sent at the end
  are dropped: one is soon stale, and the next answer gives another.
