# Deploying the memory service

The service runs on Kubernetes as one StatefulSet with one volume, which holds
the library (a git repository of markdown notes).

    nix develop -c scripts/deploy.sh devvm          build, apply, wait until it serves
    scripts/deploy.sh devvm status                  what is running, and whether it answers
    scripts/deploy.sh devvm env                     write the laptop's settings

The first argument is the kubectl context and is always given. `devvm` is the
local cluster.

## What gets created

In the namespace `carman`:

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
- **Deploy to a cluster with a registry.** Build and push the image yourself,
  set `CARMAN_IMAGE` and `CARMAN_AGENT_IMAGE`, and add a values file that says
  how the service is reached.

## The dreamer

The consolidation routine (`routines/consolidate.md`) and then the distilling
routine (`routines/distill.md`) run in the cluster, as the CronJob `dreamer`: once a night, with the consolidation
role's token and the memory command as its only tool (ADR 0027). A deploy
creates it switched off.

It needs a credential for calling a model, in the secret `carman-model`.
Put it there from whatever holds it, by a pipe, so that it is never typed or
written down (ADR 0028):

    PROGRAM-THAT-PRINTS-THE-TOKEN | scripts/deploy.sh CONTEXT secret carman-model

Then:

    scripts/deploy.sh CONTEXT dreamer run    # once, now, and print its report
    scripts/deploy.sh CONTEXT dreamer on     # let the nightly schedule run
    scripts/deploy.sh CONTEXT dreamer log    # the latest run's report
    scripts/deploy.sh CONTEXT dreamer off

A write that memory refuses (a merge that lost a number or a command of the
entries it replaced) is put right by the agent on the spot. One that is
still undone when the session ends is tried again with the refusal in hand,
by the next model in `CARMAN_MODELS`, until that ladder or `CARMAN_BUDGET_USD`
ends (ADR 0029). Both are set on the CronJob.

Every run is in memory's own log and can be undone whole:
`cmem consolidate log`, then `cmem consolidate revert RUN REASON` with the
consolidation role's token (`.local/cmem-dreamer.env`).

To try the image without the cluster, against a development service:

    docker run --rm -e CLAUDE_CODE_OAUTH_TOKEN -e CMEM_URL=http://HOST:8378 \
        -e CMEM_TOKEN=dev-dreamer "$(scripts/deploy.sh CONTEXT agent-image)" consolidate

## The egress proxy

Agent pods (those labelled `carman/role: agent`) leave the cluster through
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
The credential is one key of the secret `carman-egress`, holding the whole
header value, and gets there by a pipe:

    printf 'Bearer %s' "$(PROGRAM-THAT-PRINTS-THE-TOKEN)" | scripts/deploy.sh CONTEXT secret carman-egress KEY

A deploy refuses to go on while a named credential is missing. Tried from an
agent pod with no credential in its environment: the code host's API
answered as the token's owner, and the issue tracker's MCP endpoint accepted
an `initialize`. Without trust in carman's authority the same call is
refused by the client, as it should be.

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
    scripts/deploy.sh CONTEXT agent run NAME                    # run it once and show its report
    scripts/deploy.sh CONTEXT agent log NAME

What an agent can be given, each by a line in its entry:

- `tools: all`. Every tool of the harness, where the default is the memory
  command alone. The limits are then around the agent and not inside it: its
  pod reaches only the egress proxy and the memory service, and holds no
  credential but the model's.
- `mcp`. MCP servers by name and address. The credential for each is the
  proxy's to add.
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

The agent image has `git`, `gh`, `kubectl`, `gcloud`, `curl` and `jq`. It
works in `/work`, which is empty at the start of a run and gone at the end.
