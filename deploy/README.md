# Deploying the memory service

The service runs on Kubernetes as one StatefulSet with one volume, which holds
the library (a git repository of markdown notes).

    nix develop -c scripts/deploy.sh devvm          build, apply, wait until it serves
    scripts/deploy.sh devvm status                  what is running, and whether it answers
    scripts/deploy.sh devvm env                     write the laptop's settings

The first argument is the kubectl context and is always given. `devvm` is the
local cluster.

## What gets created

In the namespace `carman` (`CARMAN_NAMESPACE` names another):

- `statefulset/cmem`, one replica. It must stay one: the service keeps the
  library in memory and serializes writes.
- `pvc/data-cmem-0`, 1 GiB. The library is at `/data/library` inside it.
- `service/cmem`, so that anything in the cluster reaches the memory at
  `http://cmem.carman.svc:8377`.
- `secret/cmem-tokens`, made on the first deploy: one random token each for
  the laptop, worker and consolidation roles. Later deploys leave it alone.

- `cronjob/dreamer`, the nightly consolidation of the memory (see "The
  dreamer" below), switched off until it has a model credential.
- `deployment/dispatcher` and `statefulset/dispatcher-db`, the dispatcher
  and its own Postgres, with `pvc/data-dispatcher-db-0` (2 GiB),
  `secret/dispatcher-tokens` (the agent, backup and host roles' tokens, and
  the SHA-256 of the owner's, ADR 0051) and
  `secret/dispatcher-db` (the database's password), both made from random
  values on the first deploy (see "The dispatcher" below).

All of it is one Helm chart, `charts/carman` (ADR 0030), with no secret in
it. What differs between clusters is a values file: `deploy/values/local.yaml`
for the local cluster, and `CARMAN_VALUES=NAME` selects
`deploy/values/NAME.yaml`. The deploy script renders the chart and applies
it, and needs `kubectl` and `helm`. A cluster that deploys from charts by
itself can use the chart directly, with `memory.image` and `agent.image` set.

## Reaching it from the laptop

The local cluster is k3s in a Lima VM. The local values make the service a
LoadBalancer on port 8377. Lima forwards a LoadBalancer's port to the laptop,
so the laptop reaches the service at `http://127.0.0.1:8377`, on the loopback
address only. The forward appears a few seconds after the service does.

This relies on the VM's configuration as of 2026-11-18. Before that fix the
VM forwarded neither a LoadBalancer nor a NodePort, and the first deployment
here ran on the VM's own network instead.

`scripts/deploy.sh devvm env` writes three files, readable only by you:

- `.local/cmem.env`: `CMEM_URL` and the laptop role's `CMEM_TOKEN`. Source it
  in your shell profile, or copy the two values to wherever Claude Code gets
  its environment (see `adapters/claude-code/README.md`).
- `.local/cmem-dreamer.env`: the consolidation role's token, for
  `scripts/consolidate.sh`. Keep it out of ordinary sessions.

- `.local/dispatcher.env`: `DISPATCHER_URL` (`http://127.0.0.1:8379`: the
  local values make the dispatcher a LoadBalancer too), the dispatcher's
  host role's `DISPATCHER_TOKEN`, and `DISPATCHER_PLACE`, the name the
  laptop's sessions go by (ADR 0049). It is for the dispatcher's hooks in
  the laptop's own sessions; `adapters/claude-code/README.md`, "Messages to
  and from your own sessions", installs them. The deploy adds the host
  token to a token secret made before it, and restarts the dispatcher to
  read it. On a cluster whose namespace is isolated, the dispatcher's
  LoadBalancer needs `dispatcher.service.admitFrom`.

If the laptop cannot reach the service while `status` shows it running, look
at Lima's forwarding: something else on the laptop may hold port 8377 (Lima
then skips the forward and logs `failed to listen`), or the forward has gone
stale, as was seen on this machine for the API's own port. The VM's operating
manual covers both. The hook says at the start of a session when memory cannot
be reached, so this does not fail silently.

## When something goes wrong

[RECOVERY.md](RECOVERY.md) has the steps: starting it up after a restart, what
to check after an unexpected stop, backing up, restoring, and what to do when
the volume is gone.

## Looking at the library

    kubectl --context devvm -n carman exec cmem-0 -- git -C /data/library log --oneline
    kubectl --context devvm -n carman logs cmem-0

## What this does not do yet

- **Back up.** The library is one volume. On the local cluster it sits on the
  VM's state disk, so it survives a rebuild of the VM and deleting the
  instance, but not deleting the state disk. The storage class deletes a
  volume when its claim is deleted: removing the StatefulSet or the pod keeps
  the claim, removing the claim or the namespace loses the memory. Until a git
  remote exists to push to (DESIGN.md, question Q6), the backup is by hand:
  `scripts/deploy.sh devvm backup FILE`.
- **Keep its image.** The image exists only in the VM's image store. While the
  pod runs it is safe; under disk pressure the kubelet may remove an image no
  pod is using. Deploying again rebuilds it.
- **Rotate tokens.** Delete the secret and deploy again, then store the new
  tokens wherever the old ones were kept.

## Keeping the tokens in a password manager

`env` writes the tokens to files only you can read. To keep them off the disk
instead, pipe each to the program that stores it:

    scripts/deploy.sh devvm token laptop | your-password-manager ...

`token` refuses to write to a terminal. A shell function can then read the
token when `claude` starts and pass it as `CMEM_TOKEN`, with the hook settings
from the flake's `cmem-claude-code-settings` package given to `claude
--settings`.
- **A registry the cluster on your machine pulls from.** Set
  `CARMAN_REGISTRY` to its `HOST:PORT` (in `.local/bench.env`, which the
  script reads): what the script builds is pushed there and pods name it
  there. Without one the images exist only in the runtime they were built
  into, and a node that removes an image to make room cannot get it back
  for the next scheduled run.
- **Deploy to a cluster with a registry.** Set `CARMAN_IMAGE` and
  `CARMAN_AGENT_IMAGE` to pushed images, and add a values file that says how
  the service is reached. See "A shared, managed cluster" below.

## A shared, managed cluster

What the chart has for a cluster that is not yours alone (ADR 0033), each a
setting in the values:

- **Images.** `.github/workflows/images.yml` builds `cmem` and `cmem-agent`
  for amd64 and arm64 on every change to them and pushes both to the
  repository owner's registry; the run's summary prints the two references
  by digest, ready for `memory.image` and `agent.image`. Every agent
  installs the harness when it starts, with its vendor's installer and from
  its stable channel (`HARNESS_CHANNEL` in an agent's `env` names another),
  and the published agent image carries no copy of it, which is not this
  project's to hand out.
- **The warm image in a cloud registry** (ADR 0041). The job pushes to a
  repository of the cloud's private artifact registry with the pod's own
  identity, through the proxy, and the nodes pull from it with theirs. For
  Artifact Registry:

      agent:
        warm:
          enabled: true
          repository: REGION-docker.pkg.dev/PROJECT/REPOSITORY/agent
          credential: gcloud
          google: true          # the installation's identity (cloud.google), or:
          serviceAccountAnnotations: {}   # e.g. iam.gke.io/gcp-service-account: SA@PROJECT.iam.gserviceaccount.com

  The identity needs `roles/artifactregistry.writer` on the repository and
  the nodes' account `roles/artifactregistry.reader`. Give the repository a
  cleanup policy that deletes `warm-*` tags older than a week; the job also
  deletes builds beyond the newest three. The repository must be private:
  the warm image carries the vendor's harness. The tag has to exist before
  the pods name it, and must follow `agent.image`: where the deploy script
  is not used, copy `agent.image` to `REPOSITORY:warm` once (`crane copy`),
  and after each change of `agent.image` point the tag at it and run the
  job.
- **Placement and labels.** `nodeSelector` and `tolerations` place every
  pod; `podLabels` go on every pod and on the library's volume.
- **Isolation.** `isolated: true`, the default, lets only pods of the
  namespace connect to its pods. Agent pods may ask names only of the
  cluster's name service; where the nodes answer for it themselves (a
  node-local cache), list its addresses under `egress.nameServers`. The
  proxy itself goes only to public addresses (`egress.closedTo` lists the
  ranges it may not reach), so an agent cannot use it as a way to the
  cluster's other workloads or its private network.
- **Secrets.** With `externalSecrets.enabled`, External Secrets Operator
  makes the role tokens: from the cluster's store, or in the cluster itself
  with `memoryTokens.generate`. Read the laptop's once:
  `kubectl --context CONTEXT -n NAMESPACE get secret cmem-tokens -o 'jsonpath={.data.laptop}' | base64 -d | your-password-manager ...`.
  The model credential comes from the store (`modelCredential`) or by a pipe:
  `PROGRAM-THAT-PRINTS-THE-TOKEN | CARMAN_NAMESPACE=NAMESPACE scripts/deploy.sh CONTEXT secret model-credential`.
- **Backup.** `backup.enabled` with `backup.repository` copies the
  installation's state every night to a store outside the cluster (ADR
  0038): the whole library, after proving the copy can be restored, the
  dispatcher's database, the installation's definition (below), the
  conversations jobs have kept (`conversations`, with
  `agent.sessionStorage`), each interactive agent's sessions
  (`sessions-NAME`, one for every entry with `interactive: true`, so an
  agent added later is copied without more settings), and whatever
  `backup.also` names. The repository is restic's, so the store
  can be a bucket that speaks S3 (`s3:https://HOST/BUCKET/PATH`), a cloud's
  own (`gs:BUCKET:/PATH`) or a directory. Before turning it on, put the
  repository's password in the cluster, and the store's keys if it wants
  keys:

      PROGRAM-THAT-PRINTS-IT | scripts/deploy.sh CONTEXT secret backup password
      PROGRAM-THAT-PRINTS-IT | scripts/deploy.sh CONTEXT secret backup access-key-id
      PROGRAM-THAT-PRINTS-IT | scripts/deploy.sh CONTEXT secret backup secret-access-key

  Keep the password outside the cluster as well; no copy can be read
  without it. A cloud's bucket takes the identity of the job's account
  instead of keys; on a managed cluster that is an annotation
  (`backup.serviceAccountAnnotations`). A store on the installation's own
  network, which the proxy does not go to, is named under `backup.store`.
  `scripts/deploy.sh CONTEXT copies` lists what is kept and
  `scripts/deploy.sh CONTEXT restore --from-store` puts the latest library
  back (`deploy/RECOVERY.md`).

  **Conversations in the backup** (`backup.conversations`, on by default).
  An agent's sessions are what of its home `backup.sessionPaths` names: the
  harness's transcripts (`~/.claude/projects`, with each conversation's
  subagents and saved tool output), the list `agent-open` reopens at start
  (`~/.config/agent/sessions`), the harness's state (`~/.claude.json`:
  trusted directories, first-start answers) and settings, the prompt
  history, to-dos and plans, and the user-level instructions, skills,
  subagents and commands. Left out: checkouts and everything else under
  `~/work`, caches, plugins (fetched again at start from the values), the
  harness's running-process files (`~/.claude/sessions`, shell snapshots),
  the edit history used to rewind (`~/.claude/file-history`, large, and not
  needed to open a conversation), lock files and any sign-in
  (`.credentials.json`). **Work in a checkout that is not committed and
  pushed is not in the backup.** A session reopened on a restored home
  finds its conversation but an empty directory: check out the repository
  again where `agent-open` lists it, and push work in progress to a branch
  before leaving it for long.

  How it is taken: the job's pod mounts the conversations' volume and each
  agent's home (`home-agent-NAME-0`) read-only and runs `backup-state tree`
  on them, which writes a tar on standard output with names in a stable
  order, so restic finds the earlier nights' pieces of a transcript and
  stores only what was added (`scripts/check-backup-copy.sh` measures it: a
  line added to an 8 MB transcript stores under 200 kB). A transcript is
  appended to a line at a time while it is read; the copy stops at its last
  whole line, so what comes back is a conversation as it was a moment
  before the copy: the state a pod stopped at that moment leaves, which the
  harness resumes as it does after any restart (ADR 0034). Files the
  harness rewrites whole (`~/.claude.json`) are read in one go.

  Where it works: the volumes are ReadWriteOnce, which lets a second pod
  read them on the node that has them. On a cluster of one node that is
  every pod. On a cluster of several nodes the job's pod can be placed only
  where all those volumes are, which in general is nowhere: there, set
  `backup.conversations: false`, and copy them another way (a copy job per
  volume placed beside its agent's pod, or the storage's own snapshots
  copied out). A network volume that one node attaches at a time keeps the
  job's pod waiting until its deadline, and the night fails.

  Who can read what: the job's pod can now read every conversation the
  agents have had, which holds whatever they saw (files, command output,
  what the owner typed). It has no credential beyond the store's and the
  roles it had already, and nothing else runs in it. The copies are
  encrypted with the repository's password before they leave the pod; the
  store's keys alone do not read them. Whoever has the password and the
  store reads all of it, as they read the library already.
- **The installation's own definition.** Which agents run, on what
  schedule and models, what each is told to do, and the installation's
  commands are not in this repository: they are the values, routines and
  tools the deploy script reads (`.local/`, or the directory
  `CARMAN_INSTALLATION` names). Keep that directory as a git repository of
  its own, so that a change to what an agent is told is a commit with a
  reason. When it is one, each deploy puts a bundle of it, history included,
  in the cluster and the nightly job copies it as `installation`:
  `scripts/deploy.sh CONTEXT copy installation > definition.bundle`, then
  `git clone definition.bundle`, gets it back. What is not committed is
  deployed and is not in the copy. No credential belongs in it: the bundle
  holds what is committed on its branches and tags, and a deploy that finds
  one of this script's own credential files in its history puts no bundle
  in the cluster and says so.
- **The definition from within** (ADR 0056). An agent with `definition:
  true` on its entry has that bundle read-only in its pod, and so reads
  what the installation says of every agent, its hosts and its accounts by
  name. In the pod:

  ```sh
  agent-definition clone                 # ~/work/installation, as deployed
  # change it and commit, then:
  agent-definition propose PLACE/NAME    # the commits, to a session on the owner's machine
  agent-definition update                # after a deploy: the clone at what is deployed now
  ```

  On the owner's machine, whoever got the message reads it, and then:

  ```sh
  scripts/deploy.sh devvm definition apply MESSAGE-ID   # the same commits, in the definition; deploys nothing
  git -C .local log -p -1                               # read what was applied
  scripts/deploy.sh devvm                               # deploy it, as a separate step
  ```

  `apply` refuses a proposal that does not apply as it is, that touches
  git's own files, an ignored file, a link, or one of this machine's
  files, or that adds a file outside `routines/` and `tools/`. It names
  what to look at closely: a command agents run, what an agent is told, and
  `model-accounts`, whose commands run on this machine.

Every pod meets the restricted pod security standard and has its own
service account.

A cluster that deploys charts from a repository of its own takes a copy:

    scripts/chart-export.sh NAME DIR

writes the chart to `DIR/NAME` under that name. Nothing in the copy names
this project (ADR 0032), and it needs nothing else from this repository.

## The dreamer

The consolidation routine (`routines/consolidate.md`) and then the distilling
routine (`routines/distill.md`) run in the cluster, as the CronJob `dreamer`: once a night, with the consolidation
role's token and the memory command as its only tool (ADR 0027). A deploy
creates it switched off.

It needs a credential for calling a model, in the secret `model-credential`.
Put it there from whatever holds it, by a pipe, so that it is never typed or
written down (ADR 0028):

    PROGRAM-THAT-PRINTS-THE-TOKEN | scripts/deploy.sh CONTEXT secret model-credential

Then:

    scripts/deploy.sh CONTEXT dreamer run    # once, now, and print its report
    scripts/deploy.sh CONTEXT dreamer on     # let the nightly schedule run
    scripts/deploy.sh CONTEXT dreamer log    # the latest run's report
    scripts/deploy.sh CONTEXT dreamer off

A write that memory refuses (a merge that lost a number or a command of the
entries it replaced) is put right by the agent on the spot. One that is
still undone when the session ends is tried again with the refusal in hand,
by the next model in `CMEM_ROUTINE_MODELS`, until that ladder or `CMEM_ROUTINE_BUDGET_USD`
ends (ADR 0029). Both are set on the CronJob.

Beside memory, the dreamer reads the installation's definitions, so that an
entry that is one job's instruction can be told from what the job's own
definition says (ADR 0044): `agents.yaml`, each agent's routines, schedule,
models, tools and what it may reach (no environment, no credential), and the
installation's routines as `NAME.md`, in one read-only directory,
`/etc/agent/definitions`. `dreamer.definitions: false` leaves them out.

It can also be given MCP servers, as agents are, and the tools of theirs it
may call: read-only ones, for checking what an entry says about a ticket or
a document. It may call no other tool of theirs and no other server:

    dreamer:
      mcp: {tracker: "https://mcp.example.com/mcp"}
      mcpTools: {tracker: [get_issue, list_comments, list_issues]}

The proxy puts its credential on the dreamer's requests as on any agent's,
and that credential may be able to write: the list is what stops the
dreamer. Name only tools that read.

Since 2026-11-25 it can also be given commands that only read, by prefix,
each run with any arguments (ADR 0046, proposed):

    dreamer:
      commands: [gh pr view, gh pr list, gh issue view]

A prefix is a program and one to three subcommands ending in a verb that
reads (`view`, `list`, `ls`, `show`, `get`, `status`, `diff`, `log`,
`describe`, `search`, `checks`); the deploy fails on any other, so that
`gh` alone or `gh pr edit` cannot slip in.

The dreamer is told its limits at its start: what it may run, read and
call, that a shell call holds memory commands only, and that a refused call
takes nothing else away.

Every run is in memory's own log and can be undone whole:
`cmem consolidate log`, then `cmem consolidate revert RUN REASON` with the
consolidation role's token (`.local/cmem-dreamer.env`).

A report may end with questions only you can answer. The routines can run
the memory command and nothing else, so what runs them tells you: where
there is a `notify-owner` command (below), a report with questions sends
one notice, and with `agent.sessionStorage` set it names the conversation.
Answer it as the dreamer, so that the dreamer can act on the answer:

    scripts/deploy.sh CONTEXT agent resume ID

(see "Resuming an agent's conversation" below). The resumed dreamer has the
consolidation role and the memory command, as in its nightly run, and
nothing more. Opened instead from an interactive agent with `agent-resume
ID`, the conversation goes on with that agent's role, which may not do the
dreamer's work.

To try the image without the cluster, against a development service:

    docker run --rm -e CLAUDE_CODE_OAUTH_TOKEN -e CMEM_URL=http://HOST:8378 \
        -e CMEM_TOKEN=dev-dreamer "$(scripts/deploy.sh CONTEXT agent-image)" consolidate

## The egress proxy

Agent pods (those labelled `app.kubernetes.io/component: agent`) leave the cluster through
`service/egress`, an Envoy proxy in the same namespace (ADR 0031). A network
policy lets them reach the proxy, the memory service and the name service,
and nothing else. The proxy passes everything through and logs one line for
each connection:

    kubectl --context CONTEXT -n carman logs deploy/egress

Two things measured on the local cluster (k3s):

- The policy takes a few seconds to apply to a new pod. In a test, a
  connection made in a pod's first second got out directly; from five
  seconds on it was refused. Check this on each kind of cluster: one whose
  network does not enforce policies applies none, silently.
- A model call from an agent pod goes through the proxy and is logged as
  `CONNECT api.anthropic.com:443`.

For a host named under `egress.credentials` in the values, the proxy holds
the credential. It answers the agent in the host's name, with a certificate
from an authority the deploy script makes once and agent pods trust; it puts
the credential in the `Authorization` header, replacing whatever the agent
sent; and it calls the real host. Each such request is logged with its path.
The credential is one key of the secret `egress-credentials`, holding the whole
header value, and gets there by a pipe:

    printf 'Bearer %s' "$(PROGRAM-THAT-PRINTS-THE-TOKEN)" | scripts/deploy.sh CONTEXT secret egress-credentials KEY

A deploy refuses to go on while a named credential is missing. Tried from an
agent pod with no credential in its environment: the code host's API
answered as the token's owner, and the issue tracker's MCP endpoint accepted
an `initialize`. Without trust in the installation's authority the same call is
refused by the client, as it should be.

A credential that does not last (a token good for an hour, from a sign-in)
is renewed beside the proxy (ADR 0035). Under `egress.renewals`, give the
key and a command that prints the header value; it runs every `every`
seconds in a container of the proxy's pod, with a home that outlives the
pod, and the proxy reads the value again whenever it changes. Sign in once,
by hand, in that container:

    kubectl --context CONTEXT -n NAMESPACE exec -it deploy/egress -c renew-KEY -- YOUR-TOOL login

and watch it with `kubectl ... logs deploy/egress -c renew-KEY`.

A service that signs people in by OAuth in a browser, as remote MCP servers
do, needs no tool of its own. Give the renewal the command the image has for
it, and the host its key:

    egress:
      credentials:
        - {host: mcp.example.com, key: example}
      renewals:
        - {key: example, command: [oauth-credential, token, example]}

then sign in once, from a machine with a browser:

    scripts/deploy.sh CONTEXT signin example https://mcp.example.com/mcp

It prints an address to open; the browser comes back to a port of your
machine that is forwarded to the renewal, and what the sign-in yields stays
there. The renewal says in its log when the service ends the sign-in; do
the same again then. An agent reaches the service as any other, for example
as an MCP server by its address.

### The model's credential, and what each agent used

**More than one account for the model** (ADR 0055). List each in the
installation's definition, in a file `model-accounts`: a name, then the
command that prints its credential on the machine that deploys, one line
each (lines starting with `#` are comments). `scripts/deploy.sh CONTEXT
model-account` says which the proxy holds; `model-account NAME` puts
another in the proxy's secret, waits until the meter has read it (the
cluster updates the mounted file within a minute or two, and the meter
logs `the model's credential changed` with a count; no restart, so no
answer in flight is cut), writes the choice to `model-account` in the
definition (commit it), records the change in the dispatcher's log as
`model.account`, and takes a usage reading of every model on it. If the
meter does not take it within three minutes (`CARMAN_METER_WAIT`) it says
so and records nothing; `--restart` restarts the proxy instead, refused
while an agent's job runs unless `--now` follows.
Readings from before the latest change are of another account: `dispatch
status` and the admission rule leave them out. A deploy says when the
proxy holds another account than the definition chooses, and changes
nothing. `deploy/RECOVERY.md` has the same by hand.

The model's credential can be held at the proxy too (ADR 0050), by one
switch in the values:

    agent:
      modelCredential: proxy    # pod (as before), both, or proxy

- `pod`: every agent pod has the credential from `agent.modelSecret` in its
  environment, and calls to the model go through the proxy as a tunnel it
  cannot read. The chart renders exactly as before ADR 0050.
- `both`: a meter beside the proxy, in the dispatcher's image, puts the
  credential on every request to `egress.model.host`, and pods still hold
  it. For trying the proxy's way with nothing taken away.
- `proxy`: as `both`, and agent pods have a placeholder
  (`agent.modelPlaceholder`) in its place. The harness takes any value.

Going back is `pod` and a deploy; a pod made under `proxy` keeps the
placeholder until it is made again. The standing agent's pod is replaced
only by hand: it keeps whatever it had until `scripts/deploy.sh CONTEXT
agent restart NAME`.

The meter knows the caller by the pod's address, from the cluster (it may
read pods in the namespace, and reaches the cluster's API at
`agent.sandbox.api`, which the network policy needs where
`egress.closedTo` closes it). A request from an address that is no agent's
pod (no label `agent`, no `AGENT_NAME`) is refused with 403 and counted. A
header that claims another address is not believed. What each agent's pod
used, by model and kind of token, is a line in the meter's log and a
`model.usage` event in the dispatcher's log, one for each pod and model when
the pod has been quiet for `egress.model.idle` (2m) or has been asking for
`egress.model.longest` (15m):

    kubectl --context CONTEXT -n NAMESPACE logs deploy/egress -c meter
    scripts/deploy.sh CONTEXT dispatch status      # by agent, and each run against it
    scripts/deploy.sh CONTEXT dispatch log --type model.usage --last 20

`dispatch status` sets each run's own `run.usage` against the proxy's
record of the pod it ran in, and shows both where they differ. Measured
2026-11-26: they agreed to the token, for a consolidation run and for each
trial request (the hourly reading reports a cost, not tokens). The model is reached only while the proxy and its meter are
up, and the proxy's pod restarts when the dispatcher's image changes.
`egress.model.shapes: true` logs the names of the headers, events and
fields of usage that pass, never their values: how the harness was looked
at when this was built.

Services you run on the cluster for your agents (a database, a cache) are
reached directly: name their namespaces under `agent.namespaces`, and every
agent pod may reach the pods there. Everything else still goes through the
proxy or does not go.

For what a task needs and you do not run, give agents a namespace of their
own (ADR 0037):

    agent:
      sandbox:
        namespace: sandbox
        api: {addresses: ["NODE-ADDRESS/32"], port: 6443}

`api.addresses` are the addresses behind the cluster's `kubernetes` service
(`kubectl get endpointslices -n default`). An agent then has a kubectl
context named `sandbox` and may run workloads and services there, within
`agent.sandbox.quota`; they are reached as `NAME.sandbox`. Pods there cannot
touch the node, and agents cannot change the namespace's own rules. They
reach the internet only with `agent.sandbox.internet: true`, and then
directly, without the proxy's credentials. Nothing cleans it up for you.

**A trial installation there** (ADR 0054): the session that builds the
platform can stand up a small copy of it in the sandbox from a checkout
(the dispatcher with its Postgres, the memory service on an empty library,
tokens of its own and no credential of the real installation's), try a
change against it, and take it down:

```sh
scripts/trial.sh sandbox up                  # build, make the secrets, apply; about half a minute
scripts/trial.sh sandbox check               # the services answer; a probe task runs through the dispatcher
scripts/trial.sh sandbox deploy dispatch status   # the deploy script against the trial
scripts/trial.sh sandbox down                # delete everything labelled trial=trial; a few seconds
```

Trying a move from an older installation: `TRIAL_SOURCE=WORKTREE-OF-OLD
scripts/trial.sh sandbox up --old-secret`, then `scripts/trial.sh sandbox
up`. For the trial's dispatcher to start Jobs, set `agent.sandbox.trial:
true` (with `api.addresses`): it makes an account for it there with the
dispatcher's own rights, and lets it reach the cluster's API. Without it, a
trial dispatcher answers and starts nothing.

`egress.enabled: false` in the values takes the proxy and the policy out.

## The warm image

An agent's job used to spend half a minute or more installing the harness,
because its home is new each time. With `agent.warm.enabled`, a job of the
installation's own makes a warm image instead: the agent image with the
harness added as one layer, outside the home, in the installation's own
registry (ADR 0041). Agents' jobs, the dreamer and resumed conversations
start from its tag, and link that copy where the installer would have put it
in well under a second. A copy that is missing or does not run is installed
as before; an interactive agent keeps the copy in its home.

    scripts/deploy.sh CONTEXT warm run     # make it now, if the channel or the agent image changed
    scripts/deploy.sh CONTEXT warm log     # one line from each kept run: when, the version, rebuilt or not, how long

The job runs every twelve hours (`agent.warm.schedule`) and at the end of a
deploy, which first points the tag at the agent image it built. A run
asks the channel for its version (`HARNESS_CHANNEL`, stable by default) and
rebuilds only when that or the agent image changed. It adds the layer with
`crane`, under the same pod security as every agent, and deletes builds
beyond the newest three where the registry lets it.

The registry is the one the agent image is in, unless
`agent.warm.repository` names another. Where the job reaches it otherwise
than the nodes do, `agent.warm.pushRegistry` is its host as the job sees it,
`registryCIDR` and `registryPort` open it in the job's network policy, and
`insecure` allows plain HTTP. A registry on a node's own loopback, as on the
local cluster, needs to answer on an address of the node's that pods reach
too; until it does, the job fails, says so, and agents start from the agent
image and install the harness.

A registry that takes writes to these images only with a key: set
`agent.warm.credential: basic`, and put the user and the key in the secret
`agent.warm.secret` names (`registry` unless set), from a pipe:

    printf %s USER | scripts/deploy.sh CONTEXT secret registry username
    PROGRAM-THAT-PRINTS-THE-KEY | scripts/deploy.sh CONTEXT secret registry password

The deploy's own pushes are made by the machine's container tool, which is
given the key by whatever the machine uses for that; the script holds none.
Agents are given no key: what they build goes to repositories of their own.

## The dispatcher

The dispatcher (ADR 0042) starts agents' runs as Jobs and records in its log
how every Job in the namespace ended, whoever started it: a CronJob, the
dispatcher, or a person. These run its client, built from this checkout,
on this machine, as the owner (ADR 0051): with the token in
`.local/dispatcher-owner.token`, which the deploy makes and which is the
only copy (the cluster holds its SHA-256). They reach the service at its
LoadBalancer where it answers here (the local cluster's port 8379), and
otherwise through a port forwarded for the length of the command:

```sh
scripts/deploy.sh devvm dispatch status            # running, pending and why, the last runs
scripts/deploy.sh devvm dispatch status TASK       # one task
scripts/deploy.sh devvm dispatch log               # the last 50 records of the log
scripts/deploy.sh devvm dispatch log --type job.ended --last 20
scripts/deploy.sh devvm dispatch log --after 120   # what came after record 120
scripts/deploy.sh devvm dispatch submit dreamer    # a run of the CronJob `dreamer`, now
scripts/deploy.sh devvm dispatch submit agent-NAME my-run-1 --priority 5 --timeout 2h \
    --env TASK_NOTE=value --arg --flag
```

A task's class is the name of a CronJob in the namespace; its Job is made
from that CronJob's job template, named for the attempt (`my-run-1-1`), with
`TASK_ID`, `TASK_ATTEMPT` and `TASK_CLASS` in its environment. `--env` adds
variables whose names start with `TASK_`, `--arg` arguments after the
template's own; nothing else about the Job can be changed. The dispatcher
runs `dispatcher.maxRunning` tasks at once (one), highest priority first;
`status` says why a task waits. A CronJob's own schedule is untouched: the
scheduled agents still run from their CronJobs, and their runs are in the
log as `job.ended` events with `started_by: cronjob`.

Its log keeps a finished task and its events for `dispatcher.retention`
(30 days), and events that belong to no task, such as every Job's end, for
`dispatcher.retentionUnowned` (180 days).

**Its database** is the dispatcher's alone; only its pod can reach it. Where
the installation has a managed Postgres, set `dispatcher.database.host` (and
`port`, `name`, `user`, `sslmode`) and put its password in the cluster:
`... | scripts/deploy.sh CONTEXT secret dispatcher-db password`. The chart
then runs none, and the nightly backup does not copy it: that database's
own backups do.

**Its backup.** Where the chart runs the database and `backup.enabled` is
set, the nightly job copies its dump as `dispatcher` beside the library;
`scripts/deploy.sh CONTEXT copy dispatcher > dispatcher.sql` fetches the
latest. By hand:

```sh
scripts/deploy.sh devvm dispatch dump > dispatcher.sql
scripts/deploy.sh devvm dispatch restore dispatcher.sql
```

`restore` stops the service, renames the database in place to
`dispatcher_replaced_TIME` (never dropped), loads the dump into a new one,
and starts the service on it. Drop the old one when the restored one has
been looked at: `kubectl --context CONTEXT -n carman exec dispatcher-db-0 --
psql -U dispatcher -d postgres -c 'DROP DATABASE "dispatcher_replaced_TIME"'`.

**The owner's token** (ADR 0051) is on this machine only, in
`.local/dispatcher-owner.token`; keep a copy where the owner keeps
credentials. A message sent with it is the owner's word to every session.
The first deploy makes it. A deploy of an installation whose secret still
holds the token in the clear (from before ADR 0051) makes a new one, writes
it there, replaces the secret with one holding its hash and the other tokens
as they were, and restarts the dispatcher; the old token stops working. A
deploy that finds no token here, or one the cluster does not know, says so.
To make a new one (whoever may deploy may do this):

```sh
scripts/deploy.sh devvm dispatch owner-token new
```

`dispatch owner-token back` puts it on the cluster again, for going back to
a commit from before ADR 0051 (`deploy/RECOVERY.md`).

**Schedules** (ADR 0043). The dispatcher starts on time what the values
list: `dispatcher.schedules` (name, class, a five-field cron expression,
`timeZone`, `startWithin`, `timeout`, `priority`), every agent with
`dispatched: true` (on its `schedule`, in UTC), and the hourly usage
reading. A class it schedules must be a suspended CronJob, used as a
template only; `dispatched: true` renders the agent's CronJob suspended
whatever else its entry says, and the dispatcher refuses to start one that
someone has unsuspended. A schedule taken out of the values is removed when
the service starts. The scheduled agents of this chart (dreamer, backup,
warm image, any agent without `dispatched`) stay on their CronJobs.

**What did not happen.** For every CronJob it does not start, the service
expects a Job at each tick, if the CronJob was not suspended when it last
looked, within the CronJob's own catch-up (`startingDeadlineSeconds`) and
`dispatcher.expect.grace` (an hour) beyond it; the annotation
`dispatcher/grace` on a CronJob gives its own. A tick with no Job for it,
or for a later tick within that time, is recorded once as `run.missed`.

**Notices.** A failed Job, a `run.missed`, a task that did not start in
time (`task.not-started`) or ran too long (`task.overran`), and a schedule
that skipped ticks (`schedule.missed`) each give one notice: a task of
class `notify` (the CronJob `notify`, which runs the installation's
`notify-owner` with the words from the task), saying what, when, and the
command to look with. They are off until `dispatcher.notices.enabled` is
set; until then each is recorded as `notice.withheld` with its words:

```sh
scripts/deploy.sh devvm dispatch log --type notice.withheld
```

A program that tells the owner of its own failure says so with the
annotation `dispatcher/notifies-owner: "true"` on its job template; its
failure is then told only if it was killed before it could tell. A notice
that fails is in the service's log, and is not itself told.

**Usage** (ADR 0043, 0052). `agent-usage`, every hour at :07, asks each
model due for one word and records the plan's windows as the harness
reports them (`usage.observed`, one for each model). The models are every
one the values name (each agent's ladder, the dreamer's, the reading's own
`usage.model`), with `usage.addModels` and less `usage.skipModels`; the
reading's own model is read every hour, the others every
`usage.everyHours` (6) or as `usage.everyHoursFor` says. A plan can limit
one model on its own, and a call refused for a limit is recorded as that
model's reading, with the limit, its reset and the harness's words. A
model's reading failing does not stop the others. At list price a
reading costs about $0.0015 on the cheapest model and $0.03 to $0.12 on
the others; with the dreamer's ladder of three, about $0.88 a day. Every
routine run records its cost, models, attempts and tokens (`run.usage`).
`dispatch status` begins with them:

```text
usage, as read 12m ago (2026-11-27 07:00 UTC):
  the five-hour window     0%, resets 2026-11-27 10:00 UTC (in 2h48m)
  the seven-day window    70%, resets 2026-11-27 20:00 UTC (in 12h48m)
  models, most constrained first:
    claude-fable-5-2   REFUSED: seven_day_overage_included, until 2026-11-27 18:00 UTC (in 10h48m); "You've hit your limit …" (read 2h0m ago)
    claude-opus-5-5    allowed; its own limit: seven-day-opus 45%, resets 2026-11-27 20:00 UTC (read 3h0m ago)
    claude-haiku-5-5   allowed (read 12m ago)
    claude-sonnet-5-5  allowed (read 30h0m ago, OLD: it is read every 6h0m0s)
  runs, last five hours: $0.00: no runs
  runs, last seven days: $1.84: dreamer $1.84 (6 runs)
```

**Deferring what can wait** (`dispatcher.admission`, off): a task below
`below` in priority is held while a window is at `above` or more and has not
reset; `status` says why. A reading older than `maxAge` admits. A task
whose class's ladder starts with a model refused until a time to come is
held until then, whatever its priority; a refusal stands until its reset.

What it does not do yet: cancel or delete a Job, hold the agents still on
their CronJobs, or let the owner answer a notice where it arrives.

### Messages

Sessions, agents by name and the owner write to each other through the
dispatcher (ADR 0045). As the owner, from the laptop:

```
scripts/deploy.sh CONTEXT dispatch peers                  who can be written to, and whether each is busy, idle or not running
scripts/deploy.sh CONTEXT dispatch send AGENT/SESSION 'TEXT'   to a session by its name; AGENT alone starts a new conversation
scripts/deploy.sh CONTEXT dispatch inbox [--read]         what agents wrote to the owner (notices of them are withheld while notices are off)
scripts/deploy.sh CONTEXT dispatch reply ID 'TEXT'        answer one
scripts/deploy.sh CONTEXT dispatch message ID             what became of a message: delivered, how and where, read, answered
```

An agent does the same in its shell with `dispatcher ...`. Its pod has a
token the cluster makes for it for the dispatcher
(`/var/run/secrets/dispatcher/token`), which says which agent it is; the
shared agent token cannot message. The harness's hooks (in the agent
image's settings) carry a message into a running session: with the next
tool results, at the end of its turn, or by waking it when idle. A message
for a session that is not running, or for an agent by name, is a task
`message/ID` of that agent's CronJob; it waits for the running limit like
any task, and `dispatch message ID` says why. Since 2026-11-25 that
task may run as long as the agent's Job (`deadlineSeconds`) plus ten
minutes, thirty minutes where there is none, and records its cost
(`run.usage`, routine `message`). Messages are kept
`dispatcher.messages.retention` (30 days).

Who may write to an agent (ADR 0047): the owner, the agent's own sessions,
and a session answering a message it was sent; another agent only if the
agent's entry lists it:

```yaml
agents:
  - name: builder
    writtenBy: [desk]      # desk may write to builder and its sessions
```

**Since 2026-11-25 this is the default for every agent**: with no
`writtenBy`, one agent can no longer start a run of another or write to its
sessions; such a message is refused at sending, with who may write to that
agent, and recorded as `message.refused`. `dispatcher peers` marks what the
caller may not write to. A `writtenBy` that names no agent of the values
fails the render.

Going to a session from the owner's machine, from any directory (ADR
0057): the `carman` program (`nix build .#carman`), with `CARMAN_CONTEXT`
set to the cluster's kubectl context and the dispatcher's address and the
machine's token as the `dispatcher` client has them (`CARMAN_NAMESPACE`,
`CARMAN_KUBECTL` and `CARMAN_CHECKOUT` when they differ or are wanted):

```sh
carman go                    # pick one: those waiting on you first
carman go --list             # the same list, printed
carman go desk/paris-vpn     # by name; or a name alone, or the start of an id
carman go --keep NAME        # attach again after a dropped connection, until it is left
```

In the pod each attach is a view of its own of that one session's window,
with no status line and no prefix key, so it sits in a window of your own
tmux as if it ran there; two windows can show two sessions of one pod. It
is left by closing your window, or by ending the session. `agent attach
NAME` still attaches to the pod's whole tmux.

A standing agent's sessions (ADR 0053): `agent-session` starts
`dispatcher attend` in the pod (its log is `/tmp/agent-attend.log`). A
session whose harness process has gone (killed, out of memory, its window
closed) is shown as not running within about twenty seconds, and a message
to a session `agent-open` remembers opens it again in its window, on its
conversation, so that a person who attaches sees it. A session that was
forgotten is not opened: the message waits, and `dispatch message ID` says
why. The watcher knows a harness by the record its `SessionStart` hook
writes in `/tmp/agent-harness/`; a harness started before that hook gets
no verdict until it starts again. A message that a wake handed a harness
as it died is delivered a second time, to the session opened again, which
is told so; `dispatch message ID` shows `message.undelivered` between the
two deliveries.

To try it in a standing pod (after a deploy of the dispatcher and the
agent image with it, and the pod restarted):

```
pgrep -af 'dispatcher attend'                    # the watcher runs
agent-open trial-attend ~/work                   # a named session in a window of its own
ls /tmp/agent-harness/                           # its record, named by its session id
dispatcher peers | grep trial-attend             # idle
kill -9 "$(cut -d' ' -f1 /tmp/agent-harness/ID)" # its harness dies; the window keeps a shell
tail -f /tmp/agent-attend.log                    # within ~20 s: "its harness has gone"
dispatcher send AGENT/trial-attend 'say hello'   # "not open; the agent's pod opens it again"
dispatcher message MESSAGE-ID                    # within ~10 s: delivered (start)
agent-open --forget trial-attend                 # then /exit in its window, and send again:
dispatcher message MESSAGE-ID                    # waits: "not remembered in pod ..."
tmux kill-window -t main:=trial-attend
```

### A harness an agent starts itself

An agent that starts a harness of its own, to try something, gets the
image's hooks in it too: they are in the harness's user settings
(`~/.claude/settings.json`, written by `agent-env`). The memory hook would
read and write the installation's memory, or call a stand-in `cmem` on the
trial's `PATH`; the dispatcher's would register the child as a session of
the agent. Start it with the hooks off:

```
CMEM_HOOK=off claude -p 'PROMPT'                       # every hook the image installs: memory and messages
DISPATCHER_HOOK=off claude -p 'PROMPT'                 # only the dispatcher's
claude --setting-sources project,local -p 'PROMPT'     # none of the user settings: no hooks, no permissions, no env
```

Such a harness has **no model credential** of its own: the harness removes
`CLAUDE_CODE_OAUTH_TOKEN`, with the other model credentials it knows, from
the environment of every command it runs. Where the proxy holds the
model's credential (`agent.modelCredential: proxy`, ADR 0050) a child
harness needs only a placeholder, any value, and is counted as its pod's
agent:

```
CLAUDE_CODE_OAUTH_TOKEN=held-by-the-egress-proxy CMEM_HOOK=off claude -p 'PROMPT'
```

Under `pod` the pod's credential is in its first process's environment
and a trial that needs a model does not run from inside a run.

 (`scripts/deploy.sh CONTEXT agent restart NAME`); the dreamer
has none yet.

## Agents beyond the dreamer

An agent is a routine run in the agent image with what it needs to work
outside memory. Each is an entry under `agents` in the values and becomes a
CronJob `agent-NAME`; one with no schedule runs only when asked. Which
agents an installation has, the hosts its proxy holds credentials for, and
the cloud identity its agents take are the installation's own business:
they go in `.local/values.yaml`, which is git-ignored and which the deploy
script adds to the values when it is there.

    scripts/deploy.sh CONTEXT routine .local/routines/NAME.md   # put a routine in the cluster
    scripts/deploy.sh CONTEXT tool .local/tools/NAME            # put a command of your own on agents' PATH
    scripts/deploy.sh CONTEXT agent run NAME                    # run it once and show its report
    scripts/deploy.sh CONTEXT agent log NAME

### An interactive agent

An entry with `interactive: true` is not a job but a pod that stays, with a
home that outlives it (ADR 0034). The harness is not started for you: the
pod keeps a tmux session, and you work in it as on your own machine.

    scripts/deploy.sh CONTEXT agent attach NAME

attaches your terminal; detach with tmux's prefix and `d`, and whatever runs
goes on. Start the harness with `claude` in `~/work`, and after a restart of
the pod or the machine pick the conversation up with `claude --continue`.
Or let the pod do it: `agent-open NAME [DIR]` starts the harness as a named
session in a tmux window of its own and remembers it, and when the pod
starts again every remembered session is opened again on its conversation
(`agent-open` lists them, `agent-open --forget NAME` drops one, and so does
leaving a session's window with `exit` once the harness has ended). Named
sessions in one pod can find and message each other. A conversation brought
from another machine is opened once with its id, `agent-open NAME DIR ID`,
after its transcript has been copied into the home. A session `agent-open`
starts goes straight to the harness's prompt: the pod's set-up marks the
harness's first-start screens as seen, and `agent-open` marks the directory
it was asked to open as trusted, and no other. A `claude` started by hand in
another directory asks whether to trust it, as anywhere.
A deploy does not restart it, so what you have running goes on; it takes a
new image when you restart it (`scripts/deploy.sh CONTEXT agent restart
NAME`). It has the memory hook, the laptop's role in memory, and whatever
the entry gives it of the list below. `storage` sets the size of its home and
`permissionMode` how the harness treats what it is about to do (`auto`
unless said).

The shell in it is plain unless you bring your own: build an image on
`node:22-slim` with your shell, tmux configuration and tools, name it in
`CARMAN_AGENT_BASE`, and the agent image is built on it. A home that is a
volume hides what an image puts there, so have the base leave an executable
in `/usr/local/lib/agent-start.d` that links your configuration into the
home; it runs at every start.
A node removes images nothing runs when its disk fills, and the
base is one: name the command that builds it in `CARMAN_AGENT_BASE_BUILD`,
and a deploy that finds it gone makes it again.

### Telling the owner, and picking a job up

Put a command named `notify-owner` among your tools (`scripts/deploy.sh
CONTEXT tool FILE`) that sends you a message however you like to be reached,
with the recipient fixed inside it (ADR 0036). It is called as

    notify-owner --urgency now|today|whenever --importance high|normal|low [--link URL] TITLE TEXT

and every routine that runs with all tools is told to use it when something
needs you. With `agent.sessionStorage` set, a job's conversation is kept
after its pod is gone, and a notice can give you its id to carry on.

### Resuming an agent's conversation

    scripts/deploy.sh CONTEXT agent resume        # the kept conversations, and where each is open
    scripts/deploy.sh CONTEXT agent resume ID     # go on with one as its agent (the start of the id is enough)

With an id, the command finds whose conversation it is, makes a pod from
that agent's own definition (its image, environment, credentials, mounts
and account), opens the harness on the conversation there inside tmux, in
the directory the conversation was held in, and attaches your terminal
(ADR 0040). The agent goes on with its job's rights: an agent with every
tool runs without asking, as its job did; the dreamer may run the memory
command and nothing else, and what else it tries is refused rather than put
to you. The model is the one that last answered in the conversation. A
resumed dreamer reads and calls what its job could, and is told so, with
the same flags (`cmem routine --flags`).

Detach with tmux's prefix and `d`; the pod stays, and the same command
attaches to it again. Leave the harness (`/exit`) and then the shell, and
the pod ends and is removed. A conversation is open in one place at a
time: if it is open somewhere else, in another pod or in an interactive
agent, the command says where and makes nothing.

Nothing ends a pod nobody is attached to yet. To list them and remove one:

    kubectl --context CONTEXT -n NAMESPACE get pods -l conversation -L agent,conversation
    kubectl --context CONTEXT -n NAMESPACE delete job -l conversation=FULL-ID

The pod's label `conversation` has the whole id, and its annotation
`started` the time it was made. The conversation stays on the volume.

In an interactive agent, `agent-resume` lists the same conversations and
`agent-resume ID` opens one there, with that pod's rights, not the agent's:
fine for reading it, or for carrying it on as yourself. It says so and asks
before it opens another agent's conversation.

What an agent can be given, each by a line in its entry:

- `tools: all`. Every tool of the harness, where the default is the memory
  command alone. The limits are then around the agent and not inside it: its
  pod reaches only the egress proxy and the memory service, and holds no
  credential but the model's.
- `mcp`. MCP servers by name. One given an address is a remote server, and
  the credential for it is the proxy's to add. One given as a map
  (`{command: NAME}`) is run as a command, from the image or from the
  installation's own tools; whatever it calls outside goes through the proxy
  like everything else.
- `plugins`. Plugins of the harness (skills, subagents and MCP servers in
  one package), as `NAME@MARKETPLACE`, from the marketplaces listed under
  `agent.marketplaces`. They are installed when the pod starts, so a run
  has each plugin as it is that day. A plugin's own MCP servers need no
  entry under `mcp`: the proxy adds the credential for their hosts all the
  same.
- `env`. Extra environment variables, for a tool that insists on finding
  one; the value is a placeholder, never a credential.
- `hostPaths` and `groups`. Directories of the node, mounted at the same
  path, and groups of the node to put the agent in. For a cluster on your
  own machine only: a shared folder, or the socket of a build service the
  machine runs for its agents, which admits by group. What is behind a
  directory of the node is as safe as the node makes it.
- `google: true`. A cloud identity with no stored key: the pod's own token,
  signed by this cluster, is exchanged for a short-lived token of the service
  account named under `cloud.google`. The cloud side has to trust this
  cluster's token issuer first (workload identity federation, with the
  cluster's public keys uploaded). On a managed cluster of the same provider,
  its workload identity does this and `cloud.google` stays empty.
  `google: {serviceAccount: EMAIL}` gives one agent a service account of
  its own in place of the shared one. Its pods then run under an account of
  their own, `agent-NAME`, and that is the subject the service account has
  to trust: the cloud can then grant something to this agent and to no other.

What every agent should have goes under `agent.env`, `agent.hostPaths` and
`agent.groups` once, in place of a line in each entry.

A schedule is kept by the cluster, so it is only as punctual as the cluster
is awake. On a laptop that sleeps, a run whose time passed is started when
the cluster is back, if that is within the agent's `catchUpSeconds` (a day
unless said; twelve hours for the dreamer); after that it waits for its next
time. Keeping the machine awake is the machine's business.

The agent image has `git`, `gh`, `kubectl`, `gcloud`, `tofu`, `curl` and `jq`. It
works in `/work`, which is empty at the start of a run and gone at the end.
