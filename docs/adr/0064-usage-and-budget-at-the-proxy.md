# 0064: The proxy reads the account's standing from the answers it passes, and holds an agent to a limit of its own

Status: Proposed (2026-11-27). Changes ADR 0043 and ADR 0052 on when the
reading job asks a model, and ADR 0050 on what the meter writes and
refuses. Two items from the list of improvements (ADR 0058):
`usage-read-from-answers-headers` and `budget-enforced-at-the-proxy`.

## Context

**Readings.** The account's standing against its plan's limits is read by
a job that asks each model one word (ADR 0043, 0052): about a dollar a day
at list price, hourly for the cheapest model and six-hourly for the others.
A limit reached between readings is not known until an agent is refused.
Every answer of the model's API carries the standing in its headers, and
the meter beside the egress proxy (ADR 0050) sees every answer.

What the API sends, for a subscription's credential (looked at by name and
shape in an agent's pod, one call to each of four models, no credential
looked at; the harness's own schema, version 2.1.x, read beside it):

| header (`anthropic-ratelimit-unified-…`) | shape | the harness's `rate_limit_info` |
|---|---|---|
| `status` | `allowed`, `allowed_warning`, `rejected` | `status` |
| `reset` | epoch seconds | `resetsAt` |
| `representative-claim` | `five_hour`, … | `rateLimitType` |
| `W-utilization`, `W-reset`, `W-status` for W = `5h`, `7d`, and for one model `7d_oi` | a fraction, epoch seconds, a status | `unifiedWindows.five_hour`, `seven_day`, `seven_day_overage_included` (the harness's names; `7d_opus` is `seven_day_opus` by the same rule) |
| `fallback-percentage`, `overage-status`, `overage-disabled-reason`, `grace-…`, `overage-period-…` | | not in the reading |

The top-level `utilization` the harness gives is the representative
window's. A call to one model carries that model's own window (`7d_oi`
came only on the strongest model's answer), as the job's reading of it
does. So the headers carry everything the job's reading carries, for the
model that was called. What they do not carry: anything about a model
nobody called, and on a refusal the harness's words (the reading's
`refused.message`); a refusal's limit and reset are in the headers. A
request refused before it is a model's call (a malformed request, a
credential used outside the harness) has a 429 with none of these headers,
and is no reading.

**Budgets.** An agent's `budgetUSD` is enforced by the harness of each run
from what that run reports of itself: a standing agent's sessions, a child
harness and a run that lies have no limit. The meter knows what each
agent's pods used by its own count.

What the harness (2.1.x, `claude -p` against a fake API) does with each
status the meter could answer, one call each:

| status, error type | requests | what the run says |
|---|---|---|
| 400 `invalid_request_error` | 6 (tried again) | `API Error: 400 …` |
| 402 `billing_error` | 1 | `API Error: 402 …`, error `unknown` |
| 403 `permission_error` | 1 | `Failed to authenticate. API Error: 403 …`, error `authentication_failed` |
| 429 `rate_limit_error` | tried again until killed | |
| 429 with `x-should-retry: false` | 1 | `API Error: Request rejected (429) · …`, error `rate_limit`: the plan's limit to the harness and to `cmem routine` |

## Decision

**Readings at the proxy.** The meter reads the unified headers of every
answer to `POST /v1/messages` and writes `usage.observed` in the job's
form (`model`, `rate_limit_info`, `refused`), with `agent` (whose call it
was) and `seen`, under the meter's role (which may now write that type
and no other outside `model.*`). The model is the one the request asked
for, from the first 64 KiB of its body as it passes upstream; failing
that, the answer's. At most one reading a model each five minutes, and
one at once when any status (the top level's or a window's) changes or a
call is refused for a limit (a 429 whose status is `rejected`: `refused`
gets the representative limit and the reset). `METER_READINGS=off` stops
it. Everything that reads readings (status, admission, pacing) reads these
unchanged; `dispatch status` says a model's latest was seen at the proxy.

**The job reads only what is stale.** `dispatcher reading --due` asks the
dispatcher (`GET /v1/usage/latest`, the agent role) when each model was
last read since the account was last changed, and leaves out a model read
within its period less ten minutes (so that the job's own reading an hour
ago does not count). When the dispatcher cannot say, it reads every due
model. What it still costs: a model that agents call at least once in its
period is not read by the job at all; one nobody calls is read as before,
a few cents a call at list price. On an installation whose agents work
every hour, the cheapest model's hourly reading goes; the job still runs.

**A limit at the proxy.** An agent's entry (and the dreamer's values) may
have `proxyLimit: {day: N, pod: N}`, off unless set:

- The unit is a weighted token: input 1, output 5, cache write 1.25,
  cache read 0.1. These are the ratios of the API's prices, the same on
  every model, so nothing goes stale when a price changes, and one number
  is a budget. A dollar limit needs a price table that goes stale; a limit
  on raw tokens is a limit on cache reads, which are most of the tokens and
  a tenth of the cost. A token of a strong model counts as one of a cheap
  one: the limit is of work, not of money.
- `day` is all the agent's pods from 00:00 UTC; `pod` is one pod's life,
  which for a job is its run. A standing agent's pod lives for days, so
  `pod` is for jobs, and `day` for standing agents. A UTC day, not a
  rolling 24 hours, so that when it starts again is plain.
- A call over it is answered as the API answers an error, 402
  `billing_error`, with words that say what it is, which limit, set by
  whom and how to lift it, before the credential is put on. 402 is the one
  status the harness shows once as itself (see above); the words avoid
  those `cmem routine` reads as the plan's limit. The refusal is in the
  meter's log and in `model.usage` as `over_budget`.
- The owner sets, raises or lifts it without a deploy: `dispatcher limit
  AGENT --day 5M --pod 800k`, `limit AGENT off`, `limit AGENT definition`
  (an event `model.limit`, the owner's only: the meter may not write it).
  `dispatcher limit` and `dispatch status` show each limited agent's
  standing.
- The dispatcher holds the standing: the definitions' limits
  (`DISPATCHER_PROXY_LIMITS`, from the chart) under the owner's latest
  `model.limit`, and what each agent used by the log's `model.usage`
  (today's, and each pod's over two days). The meter asks for it at once
  and each half minute (`GET /v1/meter/standing`, the meter role), naming
  itself and when it started; the dispatcher leaves out that meter's
  records since then, which the meter counts itself, live, from each
  answer. So a meter that restarts takes up the day's count from the log
  at its first question, and a change of limit holds within half a minute.
  Until the first standing comes, nobody is held; a meter that cannot ask
  keeps the standing it had and goes on counting.

## Consequences

- A limit reached between the job's readings is known within seconds of
  the first answer that shows it, and admission and pacing act on it.
- The reading job's cost falls to the models nobody called in their period.
- Any agent, however it runs, can be held to a number of tokens a day or a
  run by a count that does not depend on what it says of itself.
- The limit can be overshot by the calls already in flight when it is
  reached, and by up to a tally's length (`METER_IDLE`, `METER_LONGEST`)
  of use on a second meter, which the first learns of only from the log.
- What a refused call said in words is in the job's reading and not in
  the proxy's.
- Not done: a limit in dollars; a limit per model; a rolling day; telling
  the owner when an agent is held (its run says so; the owner's status
  shows it).
