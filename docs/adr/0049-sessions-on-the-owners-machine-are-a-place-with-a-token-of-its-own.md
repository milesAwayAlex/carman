# 0049: Sessions on the owner's own machine are a place with a token of its own

Status: Accepted (2026-11-27; the owner: "i scanned them, look good at
first glance - let's test them in practice"). Built, and in use on the local
cluster from the owner's machine since 2026-11-27. Replaces the sketch in ADR 0045's
"Sessions on the laptop: not built".

## Context

Messages (ADR 0045) reach sessions in pods. The sessions that build and
operate the installation run in the harness on the owner's own machine,
outside the cluster: no pod, so no token of the cluster's making, and no
conversations volume. The owner: "sending to the host sessions (and hearing
back from them) is really important, we should make sure this works", and,
on how it gets onto the machine: "can we treat it similar to cmem? it's a
hook into carman from the local session, possibly temporary."

ADR 0045 sketched an agent name of their own whose liveness is the waiter's
connection and the last hook, reached through a `kubectl port-forward` the
hook starts. The memory service already has a simpler way in: a role token
for the laptop, and a LoadBalancer the local cluster forwards to the
laptop's loopback address. This follows the memory.

## Decision

### Who it is

The dispatcher has a fifth role, `host`, beside owner, agent, backup and
the pods' own tokens. Its token stands for one **place**, named in the
values (`dispatcher.host.place`, `host` unless changed). The place is to
everyone else an agent of that name with no job: its sessions are
`PLACE/NAME` and `session/ID`, and `PLACE` by name is the place itself.

The session's id is the one the harness gives the hook, as in a pod. Its
name is `DISPATCHER_SESSION_NAME` if set, else the last part of the
directory the session runs in (characters an address cannot hold become
`-`): the session in a checkout called `billing` is `host/billing`. The
harness does not give a hook a name a person chose for the session, and a
directory is what a person reopens a session in, so a session started again
in the same project has the same name.

The place's name may not be the name of an agent of the values (the chart
fails to render), and a pod whose agent has that name is refused as it
(`Act`): only the host token is the place.

**What is checked:** that the caller holds the host token, and so is the
place; that the place does not act as a session of a pod's agent, and no
pod acts as a session of the place.

**What is not checked, and a holder of the token can claim:**

- which of the place's sessions it is, running or not: any process with
  the token can read any of their inboxes, take their messages and send as
  them. There is no pod to tell one running session from another, so this
  is wider than in a pod (ADR 0045), where it is limited to sessions of the
  same pod, or ones that are not running;
- its session's name, directory, and whether it is busy, idle or ended;
- that it is on the owner's machine at all: the token is the place
  wherever it is used. It is kept like the memory's laptop token: in a
  git-ignored file, readable only by the owner;
- the words of a message, as everywhere.

### What it may do

These are the owner's sessions, working with the owner's rights:

- They may write to any agent by name and to any session (ADR 0047's lists
  do not limit them), including starting a run of an agent by name. The
  chain of six answers (ADR 0045) holds for them as for any session.
- An answer to one comes back to that session, not to the owner's notices.
- An agent may write to them only as ADR 0047 allows: to answer, or when
  listed under `dispatcher.host.writtenBy`, which the chart adds to
  `DISPATCHER_WRITERS` under the place's name.
- They are not the owner: a message from one to `owner` is a notice, as
  from any session; they cannot read or mark the owner's inbox; they may
  not emit events, submit tasks or read the dump. A session that receives
  a message from one is told it comes from the owner's own session, which
  works for the owner and is not the owner, and that what it asks is a
  request on the owner's behalf within the receiver's own limits.

### Delivery

The same hooks as in a pod, running the same `dispatcher` binary on the
machine: `SessionStart`, `UserPromptSubmit`, `PostToolBatch`, `Stop` and
`SessionEnd`, and the `asyncRewake` waiter at `SessionStart` and `Stop`.
What in them assumed a pod:

| Assumed | Without a pod |
|---|---|
| the token file `/var/run/secrets/dispatcher/token` | absent; `DISPATCHER_PLACE` set makes the hook use `DISPATCHER_TOKEN`, the host token. Without either, the hook does nothing |
| `AGENT_NAME` names the session's agent | the place (`DISPATCHER_PLACE`), in what the session is told |
| `DISPATCHER_SESSION_NAME`, set by `agent-open` | the directory's name, unless set |
| `AGENT_INTERACTIVE` makes a session interactive | a session on the machine is interactive (it waits at its prompt and has a waiter) unless `DISPATCHER_SESSION_KIND` says otherwise |
| `CLAUDE_ENV_FILE` | the harness gives it on the machine too: `DISPATCHER_SESSION` reaches the session's shell |
| the conversations volume, `agent-resume` | not used by the hooks; nothing on the machine is resumed by a message |

**Running** is not a pod being there. A session of the place runs while it
has not said it ended and either its waiter is connected or a hook was
heard within a day (`HostQuiet`, beyond the waiter's 23 hours). The sweep
that ends sessions whose pod has gone ends a session of the place after a
day without a word.

**A session that is not running cannot be started by a message.** The
message waits (`message.waiting/MSG/host`); the sender is told so, and that
it is delivered when that session is resumed or when a session of the same
name next starts there. A session of the place, when it starts or takes
its messages, is handed those waiting for earlier sessions there of its
name that have ended, or are idle with no waiter (a harness that died
without a word). Each message is still delivered once. A message to the
place by name waits, as one to an interactive agent by name does, until a
session there reads its inbox.

### Getting it onto a machine

- The dispatcher's Service has a type (`dispatcher.service.type`,
  ClusterIP by default). The local values make it a LoadBalancer, which
  the local cluster forwards to `http://127.0.0.1:8379` on the machine.
  Where the namespace is isolated, a LoadBalancer needs
  `dispatcher.service.admitFrom`, the addresses it is reached from, and
  the chart admits those to the dispatcher's port only; without them it
  fails to render.
- `scripts/deploy.sh CONTEXT` makes the host token with the others, and
  adds it to a token secret made before it (and restarts the dispatcher to
  read it). It is a key of its own (`host`, read as
  `DISPATCHER_HOST_TOKEN`), not an entry of `DISPATCHER_TOKENS`: a build
  from before this refuses a role it does not know, and deploying one is
  the way back of `deploy/RECOVERY.md`. `scripts/deploy.sh CONTEXT env` writes
  `.local/dispatcher.env` beside `.local/cmem.env`: `DISPATCHER_URL`,
  `DISPATCHER_TOKEN` and `DISPATCHER_PLACE`.
- `adapters/claude-code/dispatcher.settings.example.json` is the settings
  fragment, beside the memory's, and the README there says how to try it in
  one project's `.claude/settings.local.json` before installing it for
  every session, and how to take it out. A machine needs `dispatcher` on
  its PATH (the Nix package `dispatcher`); nothing else.

## Consequences

- One place per installation. A second machine would share the place and
  its token, and its sessions' names with it.
- A session on the machine that the owner closes without `SessionEnd` (a
  killed terminal) looks running for up to a day; a message to it then says
  it gets it at its next turn, and a new session of its name takes it.
- Two sessions open at once in directories of the same name share a name;
  a message to the name goes to the one seen last, as for any agent's.
- A session on the machine calls the dispatcher at every tool batch, with
  a three-second limit; a dispatcher that is down costs a tool call at most
  that.
- With the hooks installed for every session, every session on the machine
  is in `dispatcher peers`, including short ones started for a side task.
  `DISPATCHER_HOOK=off` (or `CMEM_HOOK=off`) keeps one out.

## What would make us look again

- A second machine, or a person other than the owner, with sessions of
  their own: a place each, with a token each.
- A shared cluster with no load balancer the machine can reach: the way in
  from a laptop to a service on a shared cluster is not built for the
  memory either.
- A harness that gives a hook the name a person gave the session.
