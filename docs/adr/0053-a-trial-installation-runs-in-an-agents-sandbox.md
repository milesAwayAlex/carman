# 0053: A trial installation runs in an agent's sandbox

Status: Proposed (2026-11-27).

## Context

The session that builds the platform runs in an agent's pod (ADR 0034)
with no rights over the installation's namespace, on purpose (ADR 0051).
A change to the installation itself (a migration of a secret, a new
dispatcher build, a chart change) could only be simulated there, and tried
for real only by the owner's machine deploying it to the real installation.
Every agent already has a namespace of its own for what a task needs (ADR
0037), where it may run pods, services, secrets, claims, deployments,
statefulsets, jobs and cronjobs, but not make accounts, roles or network
policies, which stay the installation's.

## Decision

**A trial installation is the chart, rendered from a checkout, in the
sandbox.** `scripts/trial.sh CONTEXT up` builds the memory service's and
the dispatcher's images from the checkout with the cluster's build service
and pushes them to the registry the sandbox's pods pull from, under names
that are the trial's (`TRIAL_NAME-memory`, `TRIAL_NAME-dispatcher`, never
the project's). It makes the secrets with the deploy script's own code
(`scripts/deploy.sh CONTEXT secrets`, with the trial's owner token in
`.local/trial/`), and applies the chart with values of its own
(`deploy/values/trial.yaml`: no egress proxy, no usage reading, no backup,
no agents). What only the installation may make (service accounts, roles,
role bindings, network policies) is left out, and every pod runs as the
namespace's default account. `check` asks the memory service and the
dispatcher whether they answer and runs a task of a class `probe` (a
suspended CronJob whose job runs `true`) through the dispatcher. `deploy`
runs the deploy script against the trial, its `dispatch` commands with the
trial's owner token. `down` deletes what the trial made, and only that.

**The chart's names are kept, and a label marks the trial's.** Every object
the trial makes carries the label `trial=TRIAL_NAME`, and its selectors are
narrowed to the label, so a trial never selects another's pods. The names
stay the chart's (`dispatcher`, `cmem`, `dispatcher-tokens`), because the
deploy script and the chart are what a trial tries, and they name those
objects. `up` refuses where an object of one of those names is in the
namespace and is not the trial's; `down` deletes by the label. One trial at
a time in one namespace.

**From another checkout.** `TRIAL_SOURCE=DIR` builds and renders from
another checkout (a worktree of an older commit), so a move from an older
installation can be tried: `TRIAL_SOURCE=OLD up --old-secret`, then `up`.

**What the sandbox does not allow, and the addition proposed.** The
dispatcher starts Jobs from CronJobs' templates with its pod's own account,
through the cluster's API. In the sandbox it runs as the namespace's
default account, which may do neither, and the sandbox's network policy
(`among-themselves`) closes the cluster's API to every pod there. So a trial
dispatcher answers, migrates, keeps its log and messages, and starts
nothing. `agent.sandbox.trial: true` makes, in the sandbox:

- a service account `trial-dispatcher`, and a Role and RoleBinding giving
  it what the dispatcher's own Role gives it in the installation's
  namespace: get, list, watch and create Jobs; get and list CronJobs; get
  and list pods;
- where `agent.sandbox.api.addresses` names the API's addresses, a network
  policy letting pods labelled `reaches-the-api: "true"` reach them on the
  API's port.

`scripts/trial.sh` runs the dispatcher as `trial-dispatcher`, labelled so,
when the account is there. This gives an agent nothing it cannot do
already: it may run a pod as any account in the sandbox, and it may create
Jobs and read pods there itself. What it adds is that a pod in the sandbox
may reach the cluster's API, with whatever rights that pod's account has.
For the default account that is what any authenticated account may do
(discovery, the issuer's public keys, its own access reviews).

**No real credential goes in.** The trial's tokens are made there and in
`.local/trial/`; there is no egress proxy, no model credential and no
credential of the real installation's.

## Not in the first version

- **A trial agent that reaches the model.** Today a model request leaves
  the cluster only through the installation's egress proxy, whose meter
  knows a caller by its pod's address and refuses a pod that is not an
  agent's of its own namespace (ADR 0050), and the sandbox's network policy
  does not reach the proxy. A later version needs one of these: the
  installation's proxy taking callers from the sandbox and naming them
  `trial` in its record (a value of the real installation, an allowance of
  the account's usage), or a proxy of the trial's own with a credential the
  owner gives it for trials (a secret in the sandbox, which every agent
  there could read). The first keeps the credential where it is and is
  preferred.
- **Messages between two trial agents.** They need agent pods with a token
  the cluster makes for the trial's dispatcher, which works with the
  addition above (the issuer's keys and pods are read through the API), and
  agent definitions in the trial's values: an agent's pod in the trial
  needs no model to send and read messages.
- **The images built from the checkout's Dockerfiles.** Where the build
  service's RUN steps cannot fetch packages (seen: the resolver they are
  given fails AAAA lookups, and Alpine's resolver then gives up on the
  name), the trial builds the same binaries on a base that already has
  what the Dockerfile installs (`alpine/git` for the memory service,
  `postgres:17-alpine` for the dispatcher), and says so.

## Consequences

- A change to the secrets, the dispatcher or the chart can be tried by the
  session that builds the platform, from its own pod, before the owner's
  machine deploys it: standing a trial up takes about half a minute (most
  of it the two image builds), taking it down a few seconds.
- The registry keeps the trial's images after `down`: nothing in the
  sandbox may delete from it.
- A trial runs the real installation's chart with a few kinds removed, so
  what those kinds do (isolation, the accounts' rights as the chart gives
  them) is not tried there.
