# 0050: The model's credential is held at the egress proxy, which records what each agent used

Status: Proposed (2026-11-26). Built; running on the local cluster with
`agent.modelCredential: proxy` since a real routine ran that way (below).

## Context

Every agent pod held the model's credential, a subscription's long-lived
OAuth token, in its environment (`CLAUDE_CODE_OAUTH_TOKEN`). The harness
hides it from the commands it runs, but any command can read it from the
first process's environment in `/proc`. Every other credential of the
installation is held by the egress proxy and put on the request there
(ADR 0031), which left the model's out: "moving model calls to a key
behind a gateway is for when the project needs it".

The owner, 2026-11-26: "the model token - let's move it to the egress, and
be sure to maintain cost attribution at the proxy level. i'm not sure if
it's better to hand agents unique placeholders or use workload identity,
but attributing inference cost is crucial."

Until now what a run cost was what the run said of itself (`run.usage`,
ADR 0043): the agent's own word, emitted with a token every agent holds.

## What was measured

On 2026-11-26, harness 2.1.326 from the warm image, Envoy 1.33.0, the
cheapest model, in throwaway pods on the local cluster.

- **The harness takes any placeholder.** With
  `CLAUDE_CODE_OAUTH_TOKEN=held-by-the-egress-proxy`, not even shaped like
  a token, it starts, sends `Authorization: Bearer` with the placeholder
  and the OAuth beta header, and works when the proxy replaces the
  header, streamed. Nothing checks the token's form before sending.
- **It sends the credential to one host.** A `-p` run (telemetry off, as
  agents run) goes to `api.anthropic.com` only: `GET
  /api/claude_code/settings`, `/api/claude_code/policy_limits`,
  `/api/claude_cli/bootstrap` (403: the credential's own scope, the same
  credential either way), `/api/directory/server-urls` (sent without a
  credential), and `POST /v1/messages?beta=true`, streamed as
  `text/event-stream`. The installer and the warm image's job reach
  `downloads.claude.ai` and the package registry with no credential, and
  plugins come from the code host with that host's. None needs the
  model's.
- **What the proxy sees.** Usage is in the answer: in a stream, the
  `message_start` event's `message.model` and `message.usage` (input,
  cache writes with their five-minute and hour breakdown, cache reads,
  output so far), and the `message_delta` event's `usage`, whose counts are
  totals so far, so the last wins; unstreamed, the body's `model` and
  `usage`. Also `server_tool_use`, `service_tier`, `inference_geo`,
  `speed`, `output_tokens_details`. The request names its session
  (`x-claude-code-session-id`) and prompt; the answer's headers carry the
  plan's windows (`anthropic-ratelimit-unified-5h-utilization`, `-reset`,
  and the same for seven days), what the hourly reading reads today.
- **The counts agree.** For each trial request, the proxy's sum by kind
  equalled the harness's own `result.usage` exactly (for example input 2,
  output 4, cache writes 3,389, cache reads 13,081). For the consolidation
  routine, see "The trial" below.
- **A request with the credential outside the harness** (curl through the
  proxy) gets 429 from the API: the subscription's credential is for the
  harness. That limits what a command could do with it; it does not make
  holding it harmless.
- **Envoy reads the caller's address from `X-Forwarded-For` by default.**
  In a trial, a pod that sent that header naming another pod's address
  was recorded as that pod. Closed (below) and tried again: refused or
  recorded as itself.

## Decision

**The proxy knows the caller by the pod's address, looked up in the
cluster.** Three ways were weighed:

| | a placeholder per agent | the pod's own token for an audience | the pod's address |
|---|---|---|---|
| Pass as another | anyone who learns it, for as long as it lives; it is in the pod's environment, so it can be read and written into memory or a message | anyone handed it, until it expires (an hour) | only by having that address: the network gives it to one pod at a time, and agents drop every capability |
| Which pod | no: an agent, not a pod | yes | yes |
| When the pod is gone | the placeholder still works | the token stops being accepted | the address is free; the record already names the pod and its UID |
| What it costs per request | a map read | a signature check, cached | a map read; an unknown address lists pods, at most once a second |
| What the harness must do | nothing new | send a token that changes every hour, from a variable read once at start: a standing pod outlives it, and the harness's other way (a helper that prints a key) changes how it signs in | nothing new |
| Made and kept | one per agent at deploy, and on a managed cluster by External Secrets | by the cluster | by the cluster |

The address wins: nothing to hold, leak or renew, the pod as well as the
agent, and the cluster's own word. Its conditions: the network must keep
a pod's address between it and the proxy's Service (it does with
kube-proxy, IPVS and the common network plugins, including those of
managed clusters); where a service mesh or another proxy sits between
pods, it does not, and the pod's own token is the way there (the meter's
`Identify` is the seam). The meter needs to read pods in its namespace (a
Role, no ClusterRole) and to reach the cluster's API, whose addresses the
network policy names (`agent.sandbox.api`). The agent is the pod's label
`agent` or its `AGENT_NAME`, as the dispatcher reads it (ADR 0045); a
pod that has neither is no agent's and is refused.

**A meter beside the proxy holds the credential.** `dispatcher meter`, from
the dispatcher's image, is a container in the proxy's pod. For the
model's host (`egress.model.host`), Envoy answers in its name with the
installation's certificate, as for other credentials, and hands every
request to the meter on loopback with the caller's address in a header
it sets itself. The address is the connection's own: the listener in
front carries `DOWNSTREAM_DIRECT_REMOTE_ADDRESS` to the inner listener,
and that listener sets `use_remote_address`, so a header that claims an
address is not believed, and adds none. The meter refuses an address that
is no agent's pod (403, in the API's own error form), puts the credential
on the rest, removes whatever the agent sent in its place, and passes the
answer on unchanged, each piece as it arrives. Envoy never has the
model's credential.

**What is recorded**: for each agent, pod and model, the requests, those
that failed, the tokens by kind as the API names them (input, output,
cache writes and their breakdown, cache reads), the server's tool use, the
sessions the requests named (the harness's word, kept beside the proxy's),
and the stretch of time; and refusals by address. Usage is read from the
answer as it passes: only the events that carry it are decoded, and
anything that cannot be read is counted as unread, never an error to the
agent. A tally is closed when its pod has asked nothing for two minutes
or has been open fifteen (`egress.model.idle`, `.longest`): a run of a
few minutes is one record per model, an hour's session four. Each closed
tally is a line in the meter's log and a `model.usage` event in the
dispatcher's log, sent every fifteen seconds, tried again while the
dispatcher does not take it (up to 5,000 records), and at most once (its
ID is derived from it).

**The meter is a role of the dispatcher's.** It has no token: the proxy's
pod presents the token the cluster makes for it for the dispatcher (ADR
0045), and the dispatcher knows it as the meter by the pod's label
`dispatcher-role: meter`, which a pod that is an agent's never is. The
meter may emit `model.*` events and nothing else; no agent or backup may
emit them. So the record does not pass through any agent and no agent can
add to it.

**`dispatch status` shows both.** Model use at the proxy by agent, over
five hours and seven days, beside what runs reported; what was refused;
and each run set against the proxy's record of the pod it ran in
(`run.usage` now names its pod), with both counts shown where they
differ by more than a token in a thousand.

**One switch: `agent.modelCredential`.** `pod` renders the chart exactly
as before this decision (checked by rendering both). `both` has the
meter put the credential on every request while pods still hold it: for
trying the proxy's way with nothing taken away. `proxy` gives agent pods
a placeholder (`agent.modelPlaceholder`) in place of the credential.
Going back is setting `pod` and deploying; a pod made under `proxy` has
the placeholder until it is made again.

**What every kind of pod gets.** Routine jobs, the dreamer, runs started
by a message, resumed conversations and the hourly reading take their
environment from the agent and dreamer templates, which the switch sets.
The standing agent's pod is replaced only by hand (`OnDelete`): it keeps
the credential in its environment until the owner restarts it, and its
requests go through the meter from the first deploy of `both`. The warm
image's job does not call the model.

## The trial

The consolidation routine, once, in a throwaway pod made from the dreamer's
CronJob template with the placeholder in place of the credential and no
memory token, against a memory service of the pod's own on a copy of the
library (`cmem backup` read from the real service; no write to it), on
2026-11-26 under `both`. One attempt, 46 seconds, $1.27 as the harness
priced it, nine requests to the model and six of the harness's own
questions. The run's own report and the proxy's record, by kind:

| | input | output | cache writes | cache reads |
|---|---|---|---|---|
| `run.usage` (the run's word) | 18 | 3,933 | 264,698 | 1,727,485 |
| `model.usage` (the proxy's) | 18 | 3,933 | 264,698 | 1,727,485 |

The proxy's record also said which model answered (the routine's ladder
started at Sonnet, not at the model its settings for that routine name;
the run's report agrees). The hourly reading ran through the meter at its
next tick, and `dispatch status` showed both records. The switch was then
set to `proxy` and deployed, back to `pod` once (the proxy rendered
without the meter, the templates with the secret, and a pod made from the
hourly reading's template reached the model through the plain tunnel),
and to `proxy` again.

## Consequences

- The model is reached only while the proxy and the meter are up. The
  proxy's pod restarts (seconds) when the dispatcher's image changes, since
  the meter is in it.
- The proxy now sees the model's prompts and answers, which were a tunnel
  it could not read. It records counts and names, never content or
  headers; its access log has the path, as for other credentials.
- The credential is in one container of the proxy's pod, mounted from
  `agent.modelSecret`; agents reach the proxy's port and nothing else in
  that pod, and the meter answers only inside it. Any pod that reaches
  the proxy's port can ask, but only an agent's pod of this namespace gets
  the credential: stricter than the other credentials, which the proxy
  puts on any caller's request.
- A new pod is known within a second of its address being given; a pod
  whose address is given again is the new pod from the moment the watch
  says so. A request in between would be recorded as the earlier pod;
  agents' pods start and ask seconds apart, and the window is that of
  the watch.
- Counting tokens and the account's own questions are counted as requests
  with no model. The hourly reading could be read from the answers'
  headers instead; not done.
- Not built: a budget the proxy enforces, and an event for each request.
- A deploy left the model unreachable for four minutes when this was
  first deployed: the deploy script's render of the proxy's hosts had
  failed silently since the warm image (it asked for images without a
  registry), so a new host got no certificate. Fixed: the render now
  stops the deploy when it fails.

**When the switch can go**: once the standing agent has been restarted
under `proxy`, and a month of the dreamer, runs started by messages and
the hourly reading has gone through the meter with no request failed by
it and no run that disagrees with its record unexplained. Then `pod` and
`both` are removed, and the credential is mounted only in the meter.
