# new-a-trial-has-a-proxy-of-its-own: A trial may have an egress proxy and meter of its own, in front of a stand-in for the model's API or chained to the installation's meter

Status: Proposed (2026-11-27).

## Context

A trial installation (ADR 0054) had no egress proxy: its values switch it
off. So a change to the meter, to the proxy's configuration, to a budget at
the proxy (ADR 0064), to the reload of a credential (ADR 0055) or to the
trials' listener itself (ADR 0065) could not be tried in a trial. Showing
ADR 0065 took a deploy of the real installation and a restart of every
agent's way to the model. A builder that tested ADR 0064 wrote a stand-in
for the model's API for the purpose and did not keep it.

## Decision

**`TRIAL_PROXY=1` gives a trial the chart's egress proxy and meter, made
from the checkout.** `scripts/trial.sh` renders the chart with the proxy
on and the model's credential at it (`agent.modelCredential: proxy`), so
the trial's agents leave through the trial's proxy exactly as an
installation's do: `HTTPS_PROXY`, the proxy's authority trusted, a
placeholder for a credential, the proxy answering in the model's host's
name and handing the request to the meter. Before the proxy starts,
`trial.sh` makes what a deploy makes for it, under the trial's names: an
authority and a certificate for the model's host, an empty secret of
other credentials, and a credential for the model that it makes up.

**What the meter sends to** (`egress.model.upstream`, new; the model's
host unless said), one of two:

- **A stand-in for the model's API**, the default: `dispatcher stand-in`,
  kept in the repository, run as a pod of the trial. It answers a message
  in the API's own shapes, whole or streamed (the events a real stream
  has, in pieces), always with the word "ok"; counts that follow from the
  request (a token for every four bytes asked, five answered), so a check
  knows what the meter should have recorded; and the headers that carry
  the plan's standing (ADR 0064). It takes one credential, the one in the
  trial's secret, read at every request, so that what the meter put on a
  request, and a changed credential, are seen. Two paths of its own are
  for a check: one sets the standing it reports (a window's utilization, a
  refusal for the plan's limit, answered 429 as the API does), one says
  what it has been asked. A real harness takes its answer as a model's
  (tried: `claude -p` against it ends well, having said "ok"), so a
  trial's agent runs end to end against it; it does no work, because
  nothing answers it but "ok".
- **The installation's meter for trials**, with `TRIAL_MODEL` as well
  (ADR 0065): the trial's meter sends on to that listener, in the clear,
  inside the cluster. The trial's proxy pod is then the caller there, and
  says so (labels `trial` and `reaches-the-model`, and the label that makes
  it the meter to its own dispatcher, `dispatcher-role: meter`): the
  installation records `trial/NAME/proxy` for the whole trial, and the
  trial's own meter records each of its agents in the trial's log. The
  installation's models and cap for trials apply.

The meter takes an upstream in the clear only where it is said
(`METER_UPSTREAM_PLAIN`, which the chart sets for an `http://` upstream):
for an address inside the cluster.

`dispatcher stand-in ask` sends one message as a harness would, through
the proxy the environment names, and prints what came back: a check's way
to ask without a harness, from the dispatcher's image.

**What the sandbox needs beyond what it has.**

- The trial's meter reads the sandbox's pods as the installation's reads
  its namespace's. It runs as the account the installation already makes
  for a trial's dispatcher (`trial-dispatcher`, ADR 0054), labelled to
  reach the cluster's API as that dispatcher is. That account may `get`
  and `list` pods and **not `watch`** them. Proposed: `watch` on pods for
  it (one verb, on what it already lists). Until then the meter's index
  falls back to listing every three seconds where watching is refused,
  which is built here and is what was tried.
- No new network policy: the trial's agents, proxy, meter and stand-in are
  pods of the sandbox, which talk to one another; chained, the proxy's pod
  goes out by ADR 0065's rule.
- What a trial cannot have: the chart's own policies for the proxy (who
  may reach it, what it may reach). A trial's proxy is open to every pod of
  the sandbox. It holds a made-up credential, or none that works outside
  the installation's cap.
- Room: the proxy's pod and the stand-in are two pods more, and the
  sandbox's quota is shared by every session's trial.

## What was tried

`scripts/check-trial-proxy.sh CONTEXT`, 2026-11-27, with no model. A pod
labelled as an agent's, with the proxy's address and authority as an
agent's pod has them, asked through the trial's proxy:

- a message, whole and streamed, to the model's host: answered "ok"; the
  stand-in had been given the meter's credential (it takes no other), not
  the pod's placeholder;
- the same pod with its `agent` label taken off: refused by the trial's
  meter, and nothing reached the stand-in;
- the trial's log had the pod's use (`model.usage`: 2 requests, 10 tokens
  out, the input the stand-in says it counts) and the account's standing
  read from the answers' headers (`usage.observed`, ADR 0064);
- the stand-in set to refuse for the plan's limit: the caller got 429, and
  the refusal was a reading in the log;
- a limit of the agent's own at the proxy, set with `dispatcher limit`
  (ADR 0064): the meter held the agent ("has used 627 of its budget of 1
  weighted tokens for today"), and let it pass again when the limit was
  lifted;
- the credential changed in the secret (ADR 0055): the meter took the new
  one up with no restart of the proxy's pod, and requests passed with it.

The first run found the stand-in wrong, usefully: it wrote a stream's
events with their fields in alphabetical order, and the meter, which
tells an event by its first bytes, read nothing of them (`unread`). The
API writes `type` first; the stand-in now does, and a test holds the
meter and the stand-in to each other.

And with a real harness: a trial agent on the installation's warm image
(`TRIAL_AGENT_BASE`), sent a message, ran through the trial's proxy to
the stand-in and answered "ok"; the trial's meter recorded its request.

Not shown: the chain to the installation's meter. The first try failed
twice over (the trial's renaming of services rewrote the installation's
address; and the label that named the trial's proxy to the installation's
meter made it an agent, not the meter, to its own dispatcher). Both are
changed here; the second needs the installation's meter from this branch
(it knows a trial's proxy by `dispatcher-role: meter`), so it is tried
after a deploy.

## Consequences

- A change to the meter or the proxy's configuration is tried in the
  sandbox by the session that makes it, in the time a trial takes to stand
  up, with nothing of the installation's touched.
- The stand-in is the API as this repository understands it: its shapes
  were written from what the meter reads and the harness accepts, and it
  will fall behind the API. A check against it shows the meter and the
  proxy agree with the stand-in, not with tomorrow's API; ADR 0065's way
  to the real model stays for that.
- The trial's authority and certificate are made by `trial.sh` with the
  same commands as the deploy script's, not by the deploy script's own
  code: a change there has to be made twice.
