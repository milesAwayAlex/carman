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

`scripts/deploy.sh devvm env` writes two files, readable only by you:

- `.local/cmem.env`: `CMEM_URL` and the laptop role's `CMEM_TOKEN`. Source it
  in your shell profile, or copy the two values to wherever Claude Code gets
  its environment (see `adapters/claude-code/README.md`).
- `.local/cmem-dreamer.env`: the consolidation role's token, for
  `scripts/consolidate.sh`. Keep it out of ordinary sessions.

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
- **Backup.** `backup.enabled` with `backup.destination: gs://BUCKET/PREFIX`
  copies the whole library there every night, after proving the copy can be
  restored. The job's account needs a cloud identity that may write to the
  bucket; on a managed cluster that is an annotation
  (`backup.serviceAccountAnnotations`). To use a copy:
  `cmem restore FILE DIR`, then `cmem serve --library DIR`.

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

Every run is in memory's own log and can be undone whole:
`cmem consolidate log`, then `cmem consolidate revert RUN REASON` with the
consolidation role's token (`.local/cmem-dreamer.env`).

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

`egress.enabled: false` in the values takes the proxy and the policy out.

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

### Telling the owner, and picking a job up

Put a command named `notify-owner` among your tools (`scripts/deploy.sh
CONTEXT tool FILE`) that sends you a message however you like to be reached,
with the recipient fixed inside it (ADR 0036). It is called as

    notify-owner --urgency now|today|whenever --importance high|normal|low [--link URL] TITLE TEXT

and every routine that runs with all tools is told to use it when something
needs you. With `agent.sessionStorage` set, a job's conversation is kept
after its pod is gone: in an interactive agent, `agent-resume` lists the
latest and `agent-resume ID` opens one in the harness, so a notice can give
you the id of the conversation to carry on.

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
- `google: true`. A cloud identity with no stored key: the pod's own token,
  signed by this cluster, is exchanged for a short-lived token of the service
  account named under `cloud.google`. The cloud side has to trust this
  cluster's token issuer first (workload identity federation, with the
  cluster's public keys uploaded). On a managed cluster of the same provider,
  its workload identity does this and `cloud.google` stays empty.

A schedule is kept by the cluster, so it is only as punctual as the cluster
is awake. On a laptop that sleeps, a run whose time passed is started when
the cluster is back, if that is within the agent's `catchUpSeconds` (a day
unless said; twelve hours for the dreamer); after that it waits for its next
time. Keeping the machine awake is the machine's business.

The agent image has `git`, `gh`, `kubectl`, `gcloud`, `tofu`, `curl` and `jq`. It
works in `/work`, which is empty at the start of a run and gone at the end.
