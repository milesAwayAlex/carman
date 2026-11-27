# 0052: The usage reading reads every model the installation uses, and a refusal is a reading

Status: Proposed (2026-11-27). Changes two decisions of ADR 0043: which
model the hourly reading asks, and what the admission rule reads.

## Context

ADR 0043's reading asks the cheapest model for one word every hour and
records the harness's last `rate_limit_event`. A plan can limit a model on
its own (a weekly limit for one model, a bucket of its own, a spend cap on
usage credits), and a call to the cheapest model does not show it: a
reading could say the five-hour window was at 0% and the seven-day at 70%,
status allowed, minutes before a session on the strongest model was refused
at its first turn until a time that evening. When the harness is refused,
the reading failed as a job and recorded nothing, though the refusal is the
standing most worth knowing.

What the harness prints when it is refused, from its own schema (version
2.1.x; no refusal could be produced to see one): the assistant message that
wraps the API's error has `error` (`rate_limit`), `is_api_error_message`,
`api_error` (`usage_limit_reached` for a plan limit or spend cap) and
`api_error_params.rate_limit_info`, in the shape `rate_limit_event`
carries, with `status: rejected`, `rateLimitType` naming the limit and
`resetsAt`; its text is the message a person sees ("… resets 6pm (UTC)").
The result has `is_error` and the same words. `rateLimitType` may be
`five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`,
`seven_day_overage_included` or `overage`; `unifiedWindows` may hold
`seven_day_overage_included`, which the harness calls a per-model bucket.

## Decision

**Which models.** The chart works out the models from the values: every
agent's ladder (or its default), a standing agent's `model` where the
values give one, the dreamer's ladders (`models`, `modelsFor`) and the
reading's own (`usage.model`); `usage.addModels` adds and
`usage.skipModels` removes. It hands the reading `USAGE_MODELS`, each
model with how many hours apart it is read.

**How often.** The job still runs every hour. The reading's own model, the
cheapest, is read every hour; every other model every `usage.everyHours`
(6), or as `usage.everyHoursFor` says, in hours spread over the day by an
offset from the model's name. At list price a reading costs about $0.0015
on the cheapest model, and about $0.03, $0.06 and $0.12 on the three
above it (the harness's own prompt, written to the cache each time, is
most of it): reading a ladder of three and the cheapest every hour would
be about $0.21 an hour, $5 a day; this schedule is about $0.88 a day.

**A refusal is a reading.** Each model's reading is its own
`usage.observed`, with its model as the subject, `every_hours`, and, when
the harness refused the call for a limit, `refused`: the limit
(`rateLimitType`, else the harness's kind of error), when it resets
(`resetsAt`, else the time in its words) and its words. The reading reads
a refusal from the error message's own record, a rejected
`rate_limit_event` beside a failed result, or the words alone, in that
order. A failure that is no limit is not a reading. One model's failing
does not stop the others; the job fails only when none was taken.

**Shown.** `dispatch status` shows the shared windows (five-hour,
seven-day) once, from the latest reading that has them, then each model by
its latest reading, most constrained first: refused (its limit, its reset,
its words), then those with a limit of their own (the fullest first), then
the rest. A reading older than its model's schedule and an hour says so.

**Used.** The admission rule (still off by default) reads the latest
reading of each model. A task whose class's ladder (`CMEM_ROUTINE_MODELS`
of its CronJob) starts with a model refused until a time to come is held
until then, whatever its priority, and says so: `cmem routine`, refused
for a limit, waits for the reset or stops, and does not climb, so a start
would only meet the same refusal. A refusal stands until the reset it
gave, however old the reading. A refused model later in the ladder does
not hold the task. The threshold on the shared windows is as before, read
from the latest reading that has them.

## Consequences

- The refusal shapes are written from the harness's schema and tested on
  made-up lines; the first real refusal should be checked against them
  (`dispatch log --type usage.observed`).
- A model's own limit shows only once it binds or is in `unifiedWindows`;
  a model whose limit is far off reads as allowed.
- A model read every six hours can be refused for up to six hours before
  the status says so; the refusal, once read, carries its own reset.
- The admission rule reads the class's CronJob for each task it is asked
  about; a dreamer routine's own ladder (`CMEM_ROUTINE_MODELS_NAME`) is not
  read, only the default one.
- The cost could be cut further by a smaller harness prompt (no tools, a
  short system prompt) for the reading; not done, untested.
