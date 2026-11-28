# 0054: A trial installation runs in an agent's sandbox

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
`.local/trial/NAME/`), and applies the chart with values of its own
(`deploy/values/trial.yaml`: no egress proxy, no usage reading, no backup,
no agents). What only the installation may make (service accounts, roles,
role bindings, network policies) is left out, and every pod runs as the
namespace's default account. `check` asks the memory service and the
dispatcher whether they answer and runs a task of a class `probe` (a
suspended CronJob whose job runs `true`) through the dispatcher. `deploy`
runs the deploy script against the trial, its `dispatch` commands with the
trial's owner token. `down` deletes what the trial made, and only that.
`list` says which trials are up in the namespace, whose, since when, and
what of its quota each holds.

**Trials stand side by side: each one's names carry its name.** Several
sessions build at once, and agents cannot make namespaces, so one
namespace holds several trials. A trial's name is `TRIAL_NAME`, or, when
that is not set, made from the agent and the session it is run from
(`AGENT_NAME` and the dispatcher's session name; off the cluster, the user
and the checkout), so it is the same for the whole session and no two
sessions share one by default. It is cut to 24 characters so that the
chart's names still fit with it. Every object the chart makes is renamed
`NAME-` and its own name (`NAME-dispatcher`, `NAME-cmem`), and so is every
reference to one in a pod (a secret, a config map, a claim, a service
reached by its name, a class in the dispatcher's schedules). The deploy
script names the objects it reads and makes with the prefix that
`CARMAN_OBJECT_PREFIX` gives (`NAME-dispatcher-tokens`,
`NAME-model-credential`), so its actions against a trial (`secrets`,
`dispatch ...`, `definition put`, `model-account`) run as they do against
an installation; it refuses to apply the chart with a prefix, which only
`trial.sh` does. A trial's classes are its CronJobs' names, so its probe is
`NAME-probe`.

A trial does not see another's objects. Every object it makes carries the
label `trial=NAME` and the annotation `trial/owner` (agent and session),
and its selectors are narrowed to the label, so it never selects another's
pods. Its dispatcher lists, watches and starts from only what carries the
label (`DISPATCHER_SELECTOR=trial=NAME`): it records the ends of its own
Jobs, knows only its own agents' pods, and refuses a class whose CronJob is
another's. `up` refuses where an object of one of its names is there and
is not the trial's. `down` deletes by the label, and takes only the
session's own trial unless told a name (`down NAME`); what the deploy
script made through `trial.sh deploy` (a model credential's secret) is
labelled there, so `down` takes it too.

The other ways considered: names kept and one trial at a time (the first
version; sessions waited for each other, and one tried its change without a
trial); a value in the chart that prefixes every name (the same result,
with every template and the real installation's render changed, for the
trial's sake only); a namespace for each trial (agents may not make one).

**When the quota is full.** The sandbox's quota (pods, requested CPU and
memory, memory limits) is shared by every trial and everything else there.
Before it applies anything, `up` adds up what the trial's pods and one task
of its would count against the quota, with the namespace's defaults where a
container says nothing, and refuses when it does not fit, saying which
resource is short and listing the trials that are up and what each holds.
Nothing is made then, and nothing of another's is taken down: the one who
started it takes down a trial of their own, or waits for another to go.

**From another checkout.** `TRIAL_SOURCE=DIR` builds and renders from
another checkout (a worktree of an older commit), so a move from an older
installation can be tried: `TRIAL_SOURCE=OLD up --old-secret`, then `up`. Its secrets are made by
this checkout's deploy script, with the prefix; a dispatcher built from a
checkout older than `DISPATCHER_SELECTOR` sees every trial's Jobs.

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
when the account is there. Every trial's dispatcher shares the account. This gives an agent nothing it cannot do
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

## Consequences

- A change to the secrets, the dispatcher or the chart can be tried by the
  session that builds the platform, from its own pod, before the owner's
  machine deploys it: standing a trial up takes about half a minute (most
  of it the two image builds), taking it down a few seconds.
- The registry keeps the trial's images after `down`: nothing in the
  sandbox may delete from it. (Since 2026-11-28 a trial's image has one
  tag, `NAME-memory:trial` and so on, which moves with each build, and the
  trial runs it by digest: the builds before the latest are untagged in
  the registry, which is what a registry's own clean-up removes; see
  deploy/README.md.)
- A trial's names are not the chart's, so a step of the deploy script that
  names an object without the prefix is not tried by a trial: it touches
  another trial's object, or none. The actions above take the prefix.
- Several trials share the sandbox's account `trial-dispatcher`; it may
  create Jobs from any trial's CronJob, and each dispatcher keeps to its
  own by its selector, not by its rights.
- A trial runs the real installation's chart with a few kinds removed, so
  what those kinds do (isolation, the accounts' rights as the chart gives
  them) is not tried there.
