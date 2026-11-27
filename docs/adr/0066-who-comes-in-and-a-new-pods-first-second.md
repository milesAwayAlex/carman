# 0066: Who comes in to the installation's pods is named, the proxy admits only agents, and a pod waits once at its start for the network to know it

Status: Proposed (2026-11-27).

## Context

Several things here had learned, each on its own, that a pod just made is
refused its first connections: a watch's job, whose every look failed
until it tried again (ADR 0063); an agent's run, whose first call to its
dispatcher was refused; a check that slept before it asked. (The meter's
wait for a new pod's address, ADR 0050, is another matter: the cluster's
API not yet showing the pod.) Asked for: the cause, measured, and one
place to handle it.

Measuring it found something worse than the thing looked for.

## What was measured

On the local cluster (k3s, one node, its own network policy controller),
2026-11-27, with a probe that dials each destination every 50 ms from a
pod's first instant for eight seconds, and says how every try went. The
pods were made in the agents' sandbox, where one policy selects every pod
(`among-themselves`: in from the sandbox's pods and from agents; out to
the sandbox's pods, the name service and, on this installation, the
internet but not the cluster's ranges). Thirteen pods.

**To a pod of its own namespace** (by its address and through its
service, the same): refused for the first 0.05 to 0.25 s, then, in most
runs, no answer at all for a further 0.3 to 0.5 s, then open. First
success between 0.10 and 1.21 s after the probe started, 0.76 s in most.
The destination's rule admits "pods of this namespace", and the new pod is
not yet one the rule knows.

**To the name service and to the outside**: open from the first try.

**To what its own rules close** (the installation's proxy on its port,
the memory, the dispatcher, the cluster's API, an agent's pod by its
address, the meter's port for trials from a pod not labelled for it):
**open from the first try, for 0.11 to 0.94 s**, and then closed as the
rule says. The agent's pod saw the sandbox pod's own address. The
dispatcher's database refused from the first try.

So two things, one cause: **the controller puts a pod's rules in place a
moment after the pod starts.** Until then the new pod is not in the sets
other pods' rules admit (it is refused where it should be let in), and its
own rules do not exist (it is let out where it should be held).

**And the second finding, from the owner's machine**: on this
installation the first second was not what kept strangers out at all. Its
values had `isolated: false`, because the owner's machine comes in through
the cluster's load balancer, and the chart's one rule for who comes in
(`only-this-namespace-comes-in`, ADR 0033) is rendered only when
`isolated` is true. A pod in a namespace with no rules of its own reached
the proxy, the memory and the dispatcher at every try for twenty seconds;
only the database, which has a rule of its own, was closed. What stood
between the sandbox and the proxy was the sandbox pods' own late rules.
The proxy puts a credential on any caller's request (ADR 0031).

## Decision

### Who comes in is named, and the local cluster is isolated like any other

`isolated: true` was already the chart's default; the local cluster's
values turned it off for want of a way to admit the load balancer. Now:

- **A service reached through a load balancer says who that is**:
  `memory.service.admitFrom` beside the dispatcher's
  (`dispatcher.service.admitFrom`, ADR 0049), each entry a CIDR as the pod
  sees the caller, or pods of another namespace by their labels
  (`{namespace, podLabels}`), for a cluster whose load balancer runs as
  pods. Isolated, a service that is not `ClusterIP` and admits nobody
  fails the render, for the memory as it did for the dispatcher.
- **The local cluster's values are isolated**
  (`deploy/values/local.yaml`), and admit k3s's service load balancer:
  its pod for each of the two services, in `kube-system`. What a pod there
  really sees is for `scripts/check-ingress.sh CONTEXT NAMESPACE source`
  to say on the cluster at hand, before isolating; it prints the entry to
  write.
- **The proxy admits only the pods that leave through it**
  (`proxy-admits-only-agents`): pods of the namespace labelled as agents'
  (`app.kubernetes.io/component: agent`: agents, the dreamer, the backup,
  the warm job, watches, the notice job), on the proxy's port; and, where
  trials reach the model (ADR 0065), sandbox pods labelled for it, on the
  meter's port for them. **Whether or not the namespace is isolated**: the
  pod that holds every credential is never open to the cluster because a
  value was off. The namespace's rule no longer selects it (as it never
  selected the database), so that being of the namespace is not enough
  either: the dispatcher and the memory have no business with the proxy.
- The rest as before: every other pod admits the namespace's pods; the
  database admits the dispatcher.

**What comes in, in all** (isolated): the namespace's own pods to each
other; the load balancer's path to the memory and the dispatcher on their
ports; agents and their like to the proxy; labelled sandbox pods to the
meter's port for trials. The node itself reaches its pods whatever the
rules say (its checks of them, `kubectl exec` and `port-forward`): that is
the cluster's doing, not a rule of ours.

### What closing ingress does for the first second, and what it leaves

The destination's rules are in place long before a stranger's pod starts,
so a new pod elsewhere is refused by the installation's pods from its
first try, as the database refused it in the measurement. The window on
the installation is closed by this, not by anything about the new pod.

What a new pod can still reach in its first second, because only its own
rules would stop it: the outside world (with nobody's credentials); the
node's own services and network; the cluster's API (where it is whoever
its account is); other namespaces' pods that have no rule of their own. An
installation that turns the sandbox's internet off should know that a pod
there has it for a second all the same. Closing that needs a network that
puts a pod's rules in place before its first process runs, or a first
container of the installation's own in every sandbox pod, which agents
would have to be made to add; neither is done here.

### A pod waits once, at its start, for the network to know it

`dispatcher ready [--for 20s] [-- COMMAND]`: asks the dispatcher and the
memory for their health and connects to the proxy, every 100 ms, until
each has answered (typically within a second), then runs COMMAND in its
place. It never fails a pod: after its time it says what did not answer
and goes on. It is run once, first:

- by `agent_setup` (`deploy/agent/agent-env`), which every agent's pod
  runs at its start: jobs, standing pods, resumed conversations, the usage
  reading;
- as the command of a lean job (a watch's: `dispatcher ready --
  dispatcher source`).

Weighed against retries in the client library: a retry there would cover
the dispatcher's own client and nothing else (not the memory's, not the
harness's first request, not a tool an agent runs), and every client would
have to tell a refusal that means "not yet" from one that means "no". At
the pod's start there is one such moment and it is known to be it. An init
container was weighed too: the same wait in a second container costs a
container's start for every job, and gains nothing over the first command
of the first.

The watch's own retries (ADR 0063) are removed. The meter's wait for a
new pod's address stays: it waits for the cluster's API, on the other
side.

## The check, and the way back

`scripts/check-ingress.sh CONTEXT NAMESPACE`, from the owner's machine,
makes a pod in a throwaway namespace, two in the installation's (one
labelled as an agent's) and two in the sandbox, and tries each way in from
each: closed to the stranger (the proxy, the memory, the dispatcher, the
database, an agent's pod, the trials' port), open to an agent, the proxy
closed to a pod of the namespace that is no agent's, the trials' port open
only to a sandbox pod labelled for it; then this machine through the load
balancer, and that every running pod is still ready (the node's checks
pass). It removes what it made.

If isolating locks the owner's machine out (the load balancer's path is
not what `admitFrom` names):

    kubectl --context CONTEXT -n NAMESPACE delete networkpolicy only-this-namespace-comes-in \
        memory-admits-the-owners-machine dispatcher-admits-the-owners-machine

A deploy puts them back; `isolated: false` keeps them away, and leaves the
proxy's own rule.

## Consequences

- Not tried on a cluster by the session that wrote it: an agent may not
  make network policies. The chart's checks pass; the owner's side applies
  it and runs the check.
- Whether this cluster's controller lets the node's checks and the load
  balancer's path through as assumed is what the check finds out. If the
  load balancer's traffic reaches a pod with the node's own address, it
  comes in whatever `admitFrom` says, and so would anything else that
  comes through the node.
- A pod of the namespace that is not labelled as an agent's can no longer
  use the proxy. Anything an installation runs beside the chart, such as a
  watcher applied by hand, needs the label.
- Every agent's pod asks three services for a sign of life before it does
  anything: a few requests, and up to twenty seconds if one of them is
  down, after which it goes on as before.
