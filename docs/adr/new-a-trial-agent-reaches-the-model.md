# new-a-trial-agent-reaches-the-model: An agent of a trial reaches the model at a listener of the meter's own for trials, known by its pod in the sandbox, with no credential there

Status: Proposed (2026-11-27).

## Context

A trial installation (ADR 0054) stands in an agent's sandbox with tokens of
its own and no credential of the real installation's. Its agents had no
model: a model request leaves the cluster only through the installation's
egress proxy, whose meter knows a caller by its pod's address in the
installation's namespace and refuses the rest (ADR 0050), and the
sandbox's network policy does not reach the proxy. ADR 0054 left it for
later, and preferred the installation's proxy taking callers from the
sandbox over a credential in the sandbox, which every agent there could
read.

On 2026-11-27 every session that built something reported the gap: a
routine, a restricted agent (ADR 0061), a run started by a rule (ADR
0063), two agents writing to each other, and the surveyor (ADR 0058) were
each tried with stand-in pods, or by a harness run from the builder's own
pod with the trial's owner's token, which skips the path being tried.

## What was measured

Harness 2.1.326, from a pod of the installation, 2026-11-27. With
`ANTHROPIC_BASE_URL` set to a plain `http://` address, a placeholder for
the credential, and no proxy variables at all (so nothing else could be
reached), `claude -p` on the cheapest model made two kinds of request to
that address and no other: `HEAD /api/hello` and `POST /v1/messages`. It
answered as usual. (The address was a forwarder in the same pod that sent
each request on through the installation's proxy, where the meter put the
credential on it.) So an agent needs one address to reach the model, and
no certificate of the proxy's.

## Decision

**The meter listens on a second port, for trials, and that port serves the
model and nothing else.** The proxy's port (3128) is not opened to the
sandbox: the proxy puts the credential of every other host it holds on
any caller's request (ADR 0031), and a pod in the sandbox may run an image
nobody here wrote. The meter's own listener for trials
(`egress.model.trials.port`, 3131) takes a request for the model's API in
the clear, inside the cluster, and does what the meter does: finds whose
the caller is, puts the credential on, passes the answer on as it comes,
records the use. No credential, and no certificate, goes into the sandbox;
an agent's pod there has a placeholder and the listener's address as
`ANTHROPIC_BASE_URL`.

**Who is calling: the address the connection comes from, looked up among
the sandbox's pods.** There is no proxy in front of this listener, so the
address is the connection's own, and a header that claims another is not
read. The meter keeps a second index of pods, of the sandbox's namespace.
A pod there is a trial's agent when it says which trial it is of and which
agent: the label `trial` (which `scripts/trial.sh` puts on everything a
trial makes) and the label `agent` or `AGENT_NAME` (as the chart gives an
agent's pod). Any other pod of the sandbox (a database, a page) is
refused, as a pod that is no agent's is in the installation's namespace.
A pod of the installation's own namespace that calls this port is not in
the sandbox's index, and is refused: its way is the proxy.

**The right the meter needs, and why it is narrow.** A Role in the sandbox
namespace, bound to the proxy's account: `get`, `list` and `watch` on
`pods`, there only. It reads; it reads pods only (not secrets, not logs);
in the one namespace where every agent may already do everything with
pods. No ClusterRole. The chart makes it, as it makes the sandbox's other
rules.

**Counted as a trial: which, and whose.** The record's agent is
`trial/TRIAL/AGENT` (for example `trial/platform-loop/surveyor`), with the
pod and its UID as for any agent, in the same `model.usage` events, so
`dispatch status` shows trials' use beside the agents' and no agent of the
installation's can be taken for one (an agent's name has no `/`). A
trial's name says whose it is: by default the agent and session that stood
it up (ADR 0054). **It is the pod's own word.** Agents make the sandbox's
pods and label them; the meter can say that a caller is a pod in the
sandbox and what that pod says of itself, not who made it. What that is
worth: trials' use cannot hide among the installation's agents' or go
unrecorded, and one trial can be told from another as long as nobody
mislabels on purpose; an agent that wanted its trial's use to read as
another's could make it so. The cap below is on all trials together for
that reason.

**What trials may spend.** Two limits at the listener, the installation's
to set, and both refuse before anything reaches the model's API, with the
reason in the answer (403, in the API's error form, which the harness
shows):

- **Which models**: `egress.model.trials.models`; unless listed, only the
  usage reading's own model, the cheapest (`usage.model`). A trial's
  agents ask for a model as any agent does (its ladder); the trial values
  in this repository name the cheapest. A request for another model is
  refused: the default is not a suggestion.
- **How much**: `egress.model.trials.requests`, messages to the model in
  24 hours, all trials together (300; a routine's run is ten to thirty).
  The meter counts in its own memory, so a restart of the proxy's pod
  forgets the count. This is the simple cap until the proxy keeps budgets
  (being built as `slice/usage-and-budget-at-the-proxy`, not landed when
  this was written): then trials are one more budget there, in tokens
  rather than requests, and this count goes.

**What the network lets through, and no more.**

- Out of the sandbox (`trial-agents-reach-the-model`, there): a pod with
  the label `reaches-the-model: "true"` may reach the proxy's pod on the
  trials' port. Not the proxy's own port, not the dispatcher, the memory
  or anything else of the installation's. The label is the trial's to
  put (trial.sh puts it on its agents' pods, not on its other pods); it
  keeps a pod that has no business with the model from reaching the
  listener by accident, and is not a guard against an agent, who may label
  a pod. The guard is the listener's own check and cap.
- Into the installation's namespace (`trial-agents-reach-the-meter`,
  where the namespace is isolated): from pods so labelled in the sandbox,
  to that port of the proxy's pod. The namespace's own pods reach the
  proxy as before. Nothing else outside comes in.
- The listener itself is in the meter's container, on the pod's address;
  the meter's first listener stays on loopback behind the proxy.

**Off unless the installation says so**: `egress.model.trials.enabled:
false`. Off, the meter has no second listener, no Role is made in the
sandbox, and no policy changes. It needs the credential at the proxy
(`agent.modelCredential` `proxy` or `both`) and a sandbox; the chart
refuses it otherwise.

**The trial's side** (`scripts/trial.sh`):

- `TRIAL_MODEL=URL`, the installation's listener
  (`http://egress.NAMESPACE.svc:3131`): each agent pod of the trial gets
  it as `ANTHROPIC_BASE_URL`, a placeholder in place of the credential,
  and the labels `reaches-the-model` and `agent`. Without it a trial's
  agents have no model, as before.
- `TRIAL_AGENT_BASE=IMAGE`, an image with the harness in it (the
  installation's warm image, ADR 0041): the trial's agent image is that
  with the checkout's binaries, scripts and routines copied over, so a
  trial's agents run what the checkout has. Nothing is installed in the
  build, and the harness is the image's copy (nothing in the sandbox
  reaches the installer).

## Consequences

- A request for the model crosses the cluster's network in the clear
  between a trial's pod and the proxy's pod, prompt and answer. Inside
  one node that is the pod network; on a cluster whose pod network is not
  trusted, the listener would want the proxy's certificate in front.
- The subscription's credential is now spent by code that was never
  merged: that is what a trial is. The models and the count bound it; the
  account's own limits are shared with everything else, and a day's
  trials at the cap on the cheapest model are a small part of them.
- The cap is one number for all trials. One session's trial can use up
  the day for the others, and the refusal says so.
- `dispatch status` shows `trial/...` rows beside the agents'. A trial's
  own dispatcher records `run.usage` in the trial's own log, and the real
  meter's `model.usage` goes to the real dispatcher: a trial does not see
  the proxy's count of itself.
- The usage reading and pacing see trials' use as part of the account's,
  with no way to tell it apart there.
- A trial's agents have no way out but the model: no egress proxy, so no
  code host, no plugins, no MCP server outside. A routine that needs
  those is still not tried in a trial.

## Left open

- The proxy's budget for trials, when it lands.
- A trial's own usage reading (its dispatcher has none), and so admission
  and pacing in a trial.
- Other hosts for a trial's agents, each a decision about a credential.
- Telling who made a pod (the cluster's audit log knows; the meter does
  not read it).
