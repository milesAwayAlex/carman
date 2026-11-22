# 0037: Agents reach the installation's services on the cluster, and have a namespace to run their own

Status: Proposed (2026-11-22). Built; running on the local cluster.

## Context

An agent's pod could reach two things: the egress proxy and the memory
(ADR 0031). Work on real code needs more than the outside world: a database
and a cache to run tests against, and now and then a service of the code's
own (an authorisation server, a storage emulator). On a person's machine
those run beside the code. For an agent in a pod they have to be somewhere
the pod can reach, and the owner asked for a place where agents start what
a task needs themselves, scheduled agents included: the point is that
agents succeed, not that they are fenced.

Two things were weighed and not built. A virtual cluster for each agent
gives it a whole API of its own, which only a task that installs
cluster-wide objects needs. A container runtime in the pod, for code that
is run from compose files, needs a privileged pod, which is the node.

## Decision

- **`agent.namespaces` names namespaces whose pods every agent may reach
  directly.** What runs there is the installation's: durable, shared, and
  not this chart's to define.
- **`agent.sandbox.namespace` is a namespace where agents run their own.**
  The chart makes it, and in it a role that lets agents' accounts create
  workloads, services, configuration and volumes and reach into them
  (logs, exec, port forwards). Agents get a kubectl context named
  `sandbox` at start. Everywhere else in the cluster they remain nobody.
- **The namespace's own rules are not the agents' to change**: its quota,
  its network policy, its roles.
- **What runs there stays in its containers and in the cluster.** The
  namespace enforces the `baseline` pod security level (no host mounts,
  host network or privileged pods), the quota bounds what may run at once
  and forbids services that take a port of the node or an outside address,
  and its pods talk only to each other and to the installation's services:
  not to the proxy, not to the rest of the cluster, not to the node's own
  network.
- **Whether they reach the internet is the installation's choice**
  (`agent.sandbox.internet`, off unless said). When on, it is direct and
  not through the proxy, so that code nobody here wrote is never handed
  the agents' credentials, and the addresses closed to the proxy are closed
  here too.
- **Agents' pods now carry their account's token**, which they did not,
  and may reach the cluster's API. The API's addresses are named in the
  values, because a network policy sees those and not the service's.
- **Everything in the sandbox is disposable.** Nothing cleans it yet.

## Consequences

- An agent that is misled can fill the sandbox up to its quota, or delete
  what another agent runs there. It cannot reach the node, the proxy's
  credentials or the memory's volume from it.
- With the internet off, a pod in the sandbox cannot fetch anything at
  run time. Images are pulled by the node, so a service that only needs
  its image works; one that downloads at start does not.
- With it on, where sandbox pods go is in no log, and an image from
  elsewhere that runs there can send out what it can read: the sandbox's
  own contents and whatever the installation's services give to anyone who
  asks. An agent can already send anything anywhere through the proxy; what
  this adds is that an image can, without the agent meaning it to.
- The installation's services trust whoever reaches them, if they are set
  up that way: every agent, and every pod an agent starts, is such a one.
- Code that is run from compose files has to be translated into manifests
  by the agent that needs it.
- The agents' account can now be presented to the cluster's API, so what
  that account may do is worth keeping to this one role.
