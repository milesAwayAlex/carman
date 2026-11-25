# 0045: Messages between sessions, agents and the owner are events in the dispatcher's log

Status: Proposed (2026-11-25). Built and tried on the local cluster with a
throwaway agent; no real agent has been sent a message.

## Context

The owner asked for an event system that carries "not only agent-to-agent
and user-to-agent communication" but those too, and then: "let's get the
messaging system built. it should be at least no worse than the existing
agent messaging system."

That system is the harness's own messaging between sessions on one
machine: a session lists its peers (name, interactive or not, busy or
idle); sends to a peer by name; the message arrives in the peer's
conversation at its next tool round if it is busy, and starts a turn at
once if it is idle; the receiver sees who it is from and replies to that
address; the sender is told when delivery fails or is refused, and can ask
to be told once when a peer next goes idle. It reaches only sessions that
are running, on the same machine, in the same pod: not a job, not a session
in another pod or whose pod is gone, not the owner.

ADR 0040 set the direction: a session (a conversation, a working directory,
a profile) is what lasts, and pods are where it runs for a while; desk pods
and runs started by events should be able to communicate and be equally
supported for continuation.

## Decision

### Addresses

| Address | Means | A message to it |
|---|---|---|
| `owner` | the owner | becomes a notice (ADR 0043), withheld while notices are off; the owner reads it with `dispatch inbox` |
| `agent/NAME` | an agent by name | starts a new conversation of that agent, from the CronJob whose pods carry the label `agent: NAME` |
| `session/ID` | a session: the harness's conversation id, wherever it runs or does not | reaches it where it runs; if it runs nowhere, resumes it as its agent |

A sender may write `NAME` for `agent/NAME`, `NAME/X` for the session of
agent NAME called X (the latest seen with that name) or whose id starts with
X, and `session/` and the start of an id. What the sender wrote is kept
beside what it resolved to.

A message to an agent by name always starts a new conversation. Handing it
to "whichever session should take it" would mean choosing among running
sessions, and the one chosen could be a session the owner is working in.
An agent with no job template (an interactive agent) has a message to it by
name wait until one of its sessions reads its inbox.

### Messages are events

A message is `message.sent` (its subject the message's id; it carries from,
the sender's agent, session name and pod, to, the address as given, body,
the message it answers, and how many answers deep it is). What became of it
is events about the same subject, whose IDs name the message, so the log's
refusal of an ID twice is what makes each happen once:

- `message.waiting/MSG/KEY`: why it waits and for which task;
- `message.delivered/MSG`: to which session, in which pod, and how
  (`tool`, `stop`, `wake`, `prompt`, `start`, `inbox`);
- `message.read/MSG`: read, by `dispatcher read`, or by answering it;
- `message.refused`: one that could not be addressed, and why.

A delivery is a claim: the dispatcher appends `message.delivered/MSG` and
hands the message over only if its append was the one that took. A message
is therefore delivered at most once, however many hooks, waiters or
dispatcher instances ask at the same moment, and a restart between sending
and delivering changes nothing. It is not delivered at least once in one
case: a hook that claims it and dies before the harness reads its output
loses it from the conversation; it stays in `dispatcher inbox`, unread.

Where each session is and what it is doing (busy, idle, ended, its pod and
the pod's UID, its name, interactive or headless) changes at every turn and
is not logged: it is kept in the dispatcher's `marks` table, one mark per
session. `session.started` and `session.ended` are logged. Nothing needed a
migration, so a build from before this runs on the same database, and the
way back of `deploy/RECOVERY.md` stays open.

Retention: messages, what became of them, and sessions' marks are kept
**30 days** (`dispatcher.messages.retention`, 720h). A task a message
became is kept as any task is (30 days after it ends). A month is long
enough to see what became of a message and to answer it; a message is not
a record to keep for half a year as usage readings are.

### Who is speaking

Every agent held the same agent token, which says nothing about which agent
it is. Now an agent's pod has a second token, made by the cluster for that
pod alone: a projected service account token whose audience is
`dispatcher`, renewed by the kubelet, at `/var/run/secrets/dispatcher/token`.
The dispatcher checks its signature against the cluster's issuer keys
(`/openid/v1/jwks`, which every service account may read), its audience,
its expiry and its namespace, and that the pod it names is there now with
the UID it names. The agent is then the pod's label `agent`, or its
`AGENT_NAME`. Both are set by whoever made the pod: the chart, the
dispatcher (from a CronJob's template) or the deploy script. Agents cannot
make or change pods in the installation's namespace.

This needs no ClusterRole: a TokenReview would, and a shared cluster may not
grant one. The dispatcher's Role gains `get` on pods. A verified token is
remembered for a minute. The shared agent token still emits events; it
cannot message.

**What is checked:** which pod is calling, and so which agent; that a
session belongs to the agent whose pod first named it; that while a session
runs in one pod (its pod is there and it has not ended), no other pod may
act as it: read its inbox, take its messages, send as it, mark its messages
read. An agent reads only messages to its own sessions and to itself by
name; the owner reads messages to the owner.

**What is not checked, and an agent can claim:**

- which of the sessions in its own pod it is: a desk pod runs several, and
  any process there can say it is any of them;
- that it is a session of its agent that is not running: any pod of the
  same agent may act as that session (that is how a run resumes it), so a
  pod of agent X can read or send as a stopped session of X;
- its session's name, and whether it is busy, idle or ended;
- the words of a message: a message says who sent it, not that what it says
  is true.

### Delivery into a running session, and what was measured

Measured on 2026-11-25 in a pod of the agent image, harness Claude Code
2.1.326, model Claude Haiku 5.5, with the credential agents use:

| Way in | Result |
|---|---|
| A channel (an MCP server declaring `claude/channel`, loaded with `--dangerously-load-development-channels server:probe`) | Refused. The harness printed "--dangerously-load-development-channels ignored (server:probe) / Channels are not currently available"; the server connected and pushed, and nothing reached the session, idle or busy. The account's organisation has not enabled channels (`channelsEnabled`), or they are not offered for this credential. |
| `PostToolBatch` hook returning `additionalContext` | Works. A message queued after the first of three tool calls was in the model's context with the next tool result; its final line did what the message asked. |
| `Stop` hook continuing the turn | Works. The turn went on with the message, at once. |
| `asyncRewake` hook started at `Stop`, exiting 2 with the message on stderr | Works on an idle session: woken within the second, and the harness itself marks the input "an automated background-task event, NOT a message from the user". |
| Typing into a tmux pane | Not needed, not built. |

So a running session gets messages through the harness's hooks, in every
session the agent image starts (the settings `agent-env` writes):

- `SessionStart`: registers the session (busy for a headless run, idle for
  an interactive one), puts `DISPATCHER_SESSION` in its shell through
  `CLAUDE_ENV_FILE`, tells it in a few lines how to message, and hands it
  what is waiting;
- `UserPromptSubmit`: busy; what is waiting comes with the prompt;
- `PostToolBatch`: what is waiting comes with the tool results (between
  tool calls);
- `Stop`: what is waiting continues the turn; otherwise the session is idle;
- `SessionStart` and `Stop`, `asyncRewake`: the waiter. It holds a
  long-poll to the dispatcher (up to 23 hours, the hook's timeout a day)
  and, when a message comes, wakes the session with it. A session that
  becomes busy ends its waiter; the next `Stop` starts another. A headless
  run has none: it ends, and a later message starts it again.
- `SessionEnd`: ended.

A hook gives up after three seconds, says nothing if the dispatcher does
not answer, and runs only where the pod has its own token: a dispatcher
that is down costs a tool call at most three seconds.

This covers the standing agent's sessions (`agent-open` names each,
`DISPATCHER_SESSION_NAME`), a resumed conversation's pod (`agent-resume
--hold`, named `resumed`), a job's routine run (named for its routine), and
a run started for a message (named `message`).

What a session is told with a message is the message in a `<message
id from sender sent>` block, then: from another agent's session, that it is
information and a peer's request through the dispatcher, not the owner's
instruction; that answering is expected; that doing more than answer is for
where it fits what the session was asked to do and its limits, which a
message does not widen; and how to answer. From the owner: that it is the
owner's word. An answer says what it answers and that an answer needs no
answer unless it asks something. One exchange between sessions has at most
six answers: two sessions that answered each other would otherwise wake
each other for ever. The first wording ("it widens neither") made the
cheapest model refuse to answer a peer's question at all; it was changed.

### Telling running from not running, and waking a session that is not

A session is running when its last word was not that it ended and the pod
it last ran in is there with the same UID. The dispatcher looks every
thirty seconds for sessions whose pod has gone without a word, records
`session.ended` for them and routes what waits for them.

A message for a session that is not running becomes a task,
`message/MSG`, of the class its agent's CronJob is, with `TASK_MESSAGE`
and `TASK_SESSION`; for an agent by name, `TASK_MESSAGE` alone. The task
goes through admission and the running limit like any other; the message
waits meanwhile, and `dispatcher message ID` says for which task and why
it has not started. A second message for the same session while its task
is open is taken by that task.

The Job runs the agent's own command, `run-routine`, which sees
`TASK_MESSAGE` and runs `agent-resume --message MSG [ID]`: it takes
ADR 0040's one-writer lock on the conversation, finds where it was held and
the model that last answered in it, takes the message (now delivered, `how:
start`), and runs the harness headless on the conversation (`claude -p
--resume ID`), or on a new one (`--session-id`), with the agent's own flags
(`cmem routine --flags`) and budget. What the run ends with is sent back as
the answer to the message, unless the session answered it by itself.

The lock is the final word on whether a session is open: if the dispatcher
thought a session was not running and it is open somewhere, the run is
refused the lock and ends, and the session gets the message where it is.

An interactive agent's conversation lives in its pod's home, not on the
conversations volume, and the agent has no job template: a message for one
of its sessions that is not open waits until it is opened again
(`agent-open` reopens remembered sessions when the pod starts).

### The owner

A message to the owner is a notice through ADR 0043's notifier: titled
with who sent it, with the text and the commands that answer it and read
it (`scripts/deploy.sh CONTEXT dispatch reply ID 'ANSWER'`,
`... dispatch inbox`). Notices are off, so it is recorded as
`notice.withheld`. The owner writes to a session or an agent with
`scripts/deploy.sh CONTEXT dispatch send TO 'TEXT'`, which runs the client
in the dispatcher's pod with the owner's token, as the other `dispatch`
commands do.

### The client

In the `dispatcher` binary, for an agent's shell (as `DISPATCHER_SESSION`,
with the pod's own token) and for the owner through the deploy script:

```
dispatcher peers                 who can be written to: the owner, agents by name, sessions busy, idle or not running, and where
dispatcher send TO 'TEXT'        delivered, waiting (and for what), or refused (exit 4)
dispatcher reply ID 'TEXT'       to whoever sent ID
dispatcher inbox [--read]        what is unread for me, delivered or not
dispatcher read ID...            mark read
dispatcher message ID            what became of a message, its events, its task, its answers
dispatcher watch SESSION         a message from the dispatcher when it next goes idle
```

`send` waits up to four seconds for a running session to take the message,
so that the sender is told "delivered" when it was.

A restricted routine (`CMEM_ROUTINE_TOOLS` other than `all`) may run
`dispatcher` beside `cmem`, where its pod has its own token, and is told so.

### Compared with the harness's own messaging

| | Harness's own | This |
|---|---|---|
| List peers, interactive or not, busy or idle | yes, in the pod | yes, across the installation, and also sessions that are not running, agents by name and the owner; "idle, not listening" when a waiter has lapsed |
| Send by name | yes | yes: by session name, id, agent or owner |
| Arrives at the next tool round when busy | yes | yes (`PostToolBatch`), or at the end of the turn (`Stop`) |
| Starts a turn when idle | yes | yes (`asyncRewake` waiter), within the second |
| Receiver sees who it is from and replies to that address | yes | yes, and is told a peer's message is not the owner's |
| Sender told of failure or refusal | yes | yes, at once, with who there is; and `message ID` shows what became of it later |
| Told once when a peer next goes idle | yes | yes (`watch`), as a message |
| A session in another pod, a job, a session whose pod is gone, the owner | no | yes |
| Survives the pod, and the dispatcher restarting | no | yes: a message is in the log until delivered |
| Delivery | as fast as the harness | one HTTP call per tool batch; a woken session waits on the dispatcher, not on a file |

Where it is still worse:

- A session idle for more than 23 hours has no waiter (the hook's timeout),
  and gets a message at its next turn; `peers` says so.
- A session's pod being there is not the harness being alive: a harness
  that died without `SessionEnd` in a pod that stays (a desk pod) looks
  running, and its messages wait for it.
- An interactive agent's session that is not open is not started by a
  message; it waits.
- Which session of a pod is speaking is not checked (above).
- The first message in a conversation started from cold re-reads the whole
  conversation at full price, as any resume does (DESIGN.md, section 4).
- Laptop sessions are not reached (below).

### Sessions on the laptop: not built

The two sessions that build this system run in the harness on the laptop.
To reach them: the client would reach the dispatcher through the deploy
script (it already runs the client in the dispatcher's pod, but a waiter's
long poll through `kubectl exec` is fragile; a `kubectl port-forward` the
hook starts is better), the laptop's settings would carry the same hooks
with the owner's token, and the dispatcher would need a principal for
them: an agent name of their own (`laptop`) whose liveness is the waiter's
connection and the last hook, not a pod. About a day's work; not done.

## What was tried on the local cluster

With a throwaway agent (`agent-msgtrial`, a suspended CronJob made with
kubectl: the agent image, the egress proxy's settings, the model
credential's reference, Claude Haiku 5.5, the memory command and the
dispatcher's client only, no memory service), its sessions in pods of its
own (the standing agent's pod was not touched; its sessions need its pod
restarted to have any of this):

1. A sends to B while B is in the middle of ten tool calls: delivered to B
   in the same second (`how: tool`); B answered with `dispatcher reply`
   before its turn ended; A, idle, was woken with the answer (`wake`) and
   did not answer it.
2. B idle at its prompt: woken (`wake`) in the same second, answered within
   one.
3. B's Job deleted, its conversation kept: the message became task
   `message/…`, a Job from the agent's template took it one second later
   (`start`), resumed B's conversation and answered from its history ("Your
   first question was: What is 17 times 3? My answer was 51 …").
4. To the agent by name: a new conversation, which answered.
5. To `nobody` and to `msgtrial/gamma`: refused at once, exit 4, naming
   the agents there are; `message.refused` in the log.
6. From A's pod as B (B running in its own pod): inbox, send, take and read
   refused (403, "session … is running in pod …, not in this one"); an
   unknown token 401.
7. B to the owner: one `notice.withheld` with the text and the reply
   command. The owner's `dispatch reply` woke B, which took it as the
   owner's word and did not answer it.
8. A's waiter stopped, the owner sent to A, the dispatcher was restarted;
   at A's next turn the message was delivered once (`prompt`) and answered.

## Consequences

- Every agent pod the chart makes has a second token, for the dispatcher;
  agents' pods carry the label `agent`, and a CronJob whose pods carry it
  can be started by a message to that agent by name. The dreamer's pod has
  neither yet: it can neither message nor be messaged.
- Real agents' runs register their sessions and call the dispatcher at
  every tool batch from their next run on. The standing agent's pod has
  none of it until it is restarted (its StatefulSet updates on delete).
- A message to an agent by name, or to a session that is not running, costs
  a run of that agent's model; the run has the agent's own budget. Nothing
  limits how many messages a session sends but the running limit.
- The trial's messages and sessions are in the local cluster's log until
  retention removes them.
- There is no requirement in `REQUIREMENTS.md` for messaging between
  sessions, agents and the owner beyond R2's "a running task can receive
  later events addressed to it"; the tests name R2. Proposed wording for the
  owner to add, if wanted: "R11 | Sessions, agents and the owner can write
  to each other | A message reaches its addressee whether its session is
  busy, idle or not running, once; the sender can see what became of it."
- Added 2026-11-25: a run started for a message may run as long as its
  agent's Job (the job template's `activeDeadlineSeconds`) plus the ten
  minutes ADR 0043 gives a dispatched agent, and thirty minutes only where
  the template has no deadline. It records one `run.usage` (routine
  `message`, with the message's id) through `cmem routine --usage-of`, the
  code a routine's run uses.

## What would make us look again

- Channels enabled for the account: the waiter could become a channel
  server, which the harness made for this.
- The harness offering a way in for an idle session that is not a hook.
- Many sessions: one HTTP call per tool batch each, and one long poll per
  idle session, on one dispatcher.
