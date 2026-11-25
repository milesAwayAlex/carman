# 0042: The dispatcher runs as a service that starts Jobs from CronJob templates and records how every Job ended

Status: Proposed (2026-11-24). Built and tried on the local cluster with a
template made for the trial; no real agent has been started through it.

## Context

The dispatcher existed as a library (ADR 0039): a contract, an acceptance
suite and an implementation on Postgres. Nothing ran it. Agents run as
CronJobs (`agent-NAME`, `dreamer`, `backup`), and "run this agent now" is
`kubectl create job --from=cronjob/NAME`. Nobody hears of a run that failed
or did not happen unless the run itself says so.

This decision makes the dispatcher a running service on the cluster that
starts real work and records what happened. It is a mechanical controller:
no model is called inside it, now or later. Schedules by time of day,
notices to the owner about what did not happen, and readings of the
account's usage come next, and are not part of this decision.

## Decision

### One binary, service and client

`dispatcher` (`cmd/dispatcher`), named for what it is (ADR 0032): `dispatcher
serve` opens `pgxdispatch` on its database, starts the job watcher, and
serves an HTTP API on port 8379; the other commands (`submit`, `emit`,
`status`, `log`, `dump`) are its client, reading `DISPATCHER_URL` and
`DISPATCHER_TOKEN` as `cmem` reads its own. The API (`internal/dispatch/api`):

| Route | What | Roles |
|---|---|---|
| `POST /v1/events` | emit an event | owner, agent |
| `GET /v1/events` | the log after a sequence number, or its last records, by type | owner |
| `POST /v1/tasks` | submit a task | owner |
| `GET /v1/tasks?state=` | tasks in a state, with why each is not running | owner |
| `GET /v1/tasks/{id}` | one task's state and reason | owner |
| `GET /v1/dump` | the database as SQL | owner, backup |

### Roles

A bearer token per role, made by the deploy script from random values.

- **owner**: everything. The owner's commands run the client inside the
  service's pod, which has the owner's token; no port of the laptop is
  opened. A scheduler, when built, gets a role of its own.
- **agent**: any agent or watcher. It may only emit, and not events of the
  types the dispatcher and its job watcher write (`task.*`, `job.*`,
  `schedule.*`): an agent can say what it saw, not that work started or
  ended. Its event's ID is prefixed `agent/` and its source `agent/`, so it
  cannot take, ahead of time, an ID the dispatcher will write and so
  suppress it. It cannot submit or read. Agents are not given this token
  yet; the route and the network rule that lets them reach the service are
  there for when they are.
- **backup**: reads the dump, nothing else.

### Starting work: a Job from a CronJob's template

A task's class names a CronJob in the namespace; the starter
(`internal/dispatch/kube`) reads its job template and creates a Job from
it, as `kubectl create job --from` does, with:

- a name derived from the attempt (`TASK-N` when the task's ID is already a
  valid name, else as much of it as is valid with a hash), so that a second
  start of the same attempt finds the Job and is a success, and a Job of
  that name that carries another attempt is an error;
- the task and attempt as labels (`dispatcher/task`, `dispatcher/attempt`,
  in the characters a label allows) and exactly as annotations;
- `TASK_ID`, `TASK_ATTEMPT`, `TASK_CLASS` (and `TASK_WAKE` for a woken
  attempt) in the first container's environment.

A task's payload may add environment variables whose names start with
`TASK_`, and arguments after the template's own; it may not set a variable
the template sets, nor anything else about the Job (image, command,
account, volumes). The service refuses such a payload, and a class that
names no CronJob, when the task is submitted, so neither waits in the queue
failing to start. The Job has no owner reference: the CronJob's history
limit and concurrency policy do not apply to it, and its template's
`ttlSecondsAfterFinished` does.

Any CronJob in the namespace can be a class; only the owner can submit.

The service talks to the API through `net/http` as its pod's own account:
create, read and watch Jobs, read CronJobs, list pods. A client library
would add tens of modules for these four calls. Its Role grants exactly
those verbs, in its own namespace; it deletes nothing, and cannot cancel
work.

### Hearing how work ended: the watcher

The watcher lists every Job in the namespace on start and every ten
minutes, and watches between. For each Job that has ended (its `Complete`
or `Failed` condition) it emits one `job.ended` event: what ran (the
CronJob, or the task's class) as the subject, who started it (`cronjob`,
`dispatcher`, `by-hand`), the outcome and the Job's reason, when it started
and ended, and the exit code of its last pod when the pod is still there.
For a Job the dispatcher started it also emits the attempt's
`task.finished`, done or failed.

Each event's ID is derived from the Job's UID (`job.ended/UID`,
`task.finished/UID`), and the log refuses an ID twice, so a Job is recorded
once however often the service restarts or a watch repeats itself. A Job
that ended while the service was down is found by the list when it comes
back, as long as the cluster still has it: agents' Jobs are kept three days
(`agent.keepFinishedSeconds`).

Every run of every agent is then in the log whether the dispatcher started
it or a CronJob did. That is what makes "a job did not run, and nobody
knew" visible to what follows.

### Its database

The dispatcher owns its Postgres (ADR 0039), as the memory service owns its
library. The chart runs one of its own, `postgres:17-alpine` with a volume,
that only the service's pods can reach (a network policy; the namespace-wide
ingress rule of a shared cluster excludes it). The password is made by the
deploy script from a random value, through a pipe; the service and
`pg_dump` read it from libpq's `PGPASSWORD`, so it is in no address. An
installation with a managed database gives its address in
`dispatcher.database.host` and its password in the secret, and the chart
runs none.

Retention: finished tasks and their events are kept **30 days** after they
end; events that belong to no task (every Job's end, and later the usage
readings) **180 days**. A month of task history is enough to explain what
happened to a task; half a year of runs shows a pattern of what did not
happen, and usage readings are wanted for months. `pgxdispatch` gained
`Options.RetentionUnowned` for the second period.

### Its backup

When the chart runs the database, its dump is added to the nightly backup's
list (ADR 0038, `backup.also`). The dump is made by the service, with a
`pg_dump` of the server's major version in its own image, and handed to
the backup over HTTP as the backup role. The backup job thus needs no
database client and no route to the database, which stays reachable by the
service only. The service writes the whole dump to a file before sending
it, with its length, so a dump that failed half way is a failed copy, not a
short one.

`scripts/deploy.sh CONTEXT dispatch restore FILE` stops the service,
renames the database in place aside, loads the dump into a new one, and
starts the service on it.

## Consequences

- A second Postgres beside the memory, with its version to keep (the
  server's image and the client in the service's image move together).
- The watcher records the end of Jobs it did not start, including those of
  the deploy script's own (the warm image's job, the pods `copies` makes).
  They are runs too; the subject and `started_by` tell them apart.
- A task that overruns is marked failed but its Job goes on: the service
  cannot delete Jobs. A Job removed before the watcher saw it end (the
  service down for longer than a Job is kept) leaves its task running until
  its attempt timeout fails it.
- The trial tasks run on the local cluster are in its log and stay until
  retention removes them.

## What would make us look again

- Agents needing to wait for an event (ADR 0039's waits): an agent would
  emit its own `task.finished` with `waiting` before it exits, which the
  agent role cannot do yet.
- A second kind of template (a Job spec kept in a ConfigMap, a pod of a
  standing agent) or cancelling work: the Role grows with it.
- More than a few hundred Jobs in the namespace: the list every ten minutes
  reads them all.
