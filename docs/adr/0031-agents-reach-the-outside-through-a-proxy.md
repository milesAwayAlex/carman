# 0031: Agents reach the outside through a proxy that holds their credentials

Status: Accepted (2026-11-19). Built: the proxy, the network policy, and
credentials attached for named hosts. Not built: a cloud provider token by
identity, and rules on what a credential may be used for.

## Context

The dreamer needs one thing from outside the cluster, a model. The next
agent needs a code host, an issue tracker, a cost tool and a cloud
provider's APIs, each with a credential. ADR 0028 says carman holds its own
credentials as secrets in its cluster; it left open how a credential gets
onto an agent's request, which is the hard part.

The owner's bar: at least as safe as the same harness run on a laptop,
where the agent can read every token the person has. Better where it is
cheap. The harness's own deployment guide recommends the same shape as the
owner had in mind: a proxy outside the agent's boundary that adds
credentials to outgoing requests, so that the agent never has them, and
that records where the agent went.

What was found by looking and by trying:

- The services the first task needs each accept a static key in an
  `Authorization` header, including on their hosted MCP endpoints. A remote
  MCP server is an HTTPS endpoint like any other. The harness does not
  start a sign-in when its configuration already supplies that header.
- Envoy has a filter that adds a credential from a secret to a request.
- The harness can sign in to an MCP server that only offers an interactive
  sign-in from a machine with no browser: it prints an address to open
  elsewhere and takes the redirect back by paste. The tokens are then a
  file in its home directory.
- MCP gateways exist that hold keys and restrict tools, and others that do
  the sign-in and keep the tokens fresh. None is needed for a service that
  takes a static key.

## Decision

- **One egress proxy, Envoy, in carman's namespace, in the chart.** Agent
  pods are labelled as agents, and a network policy lets such a pod reach
  the proxy, the memory service and the cluster's name service, and nothing
  else.
- **It does not restrict where an agent may go.** Everything is passed
  through and logged: when, from which pod, to which host, how it ended and
  how much was sent. A list of allowed hosts would have to be kept ahead of
  every new use, such as searching the web. What is restricted is where a
  credential may go.
- **For the hosts it has a credential for, the proxy answers in the host's
  name**, with a certificate from carman's own authority that agent pods
  trust, adds the credential, and calls the real host. The agent is
  configured with a placeholder. A real credential can therefore only ever
  be sent to its own service.
- **The model credential is not behind the proxy.** It is a subscription
  token given to the harness in its environment, as on a laptop. Moving
  model calls to a key behind a gateway is for when the project needs it.
- **Cloud provider APIs are reached by identity, not by a stored key.** On
  a managed cluster the provider's workload identity gives the pod a
  short-lived token for a service account that can only read. Run locally,
  the owner's own command-line login mints an hour-long token for that same
  account by impersonation, and that token is what the cluster is given.
  The person's own identity, which can change production, is never
  forwarded.
- **No MCP gateway and no interactive sign-in until a task needs one.**
  When one does: a gateway that does the sign-in and holds the tokens is
  the better answer, and the harness's own sign-in on a volume is the one
  that works today.

## Consequences

- The proxy sees the content of calls to the hosts it adds credentials for.
  It logs only where a call went.
- An agent can still do anything its credentials allow on a host it is
  given. Limits of that kind belong on the credential (a key for one
  repository, a role that only reads) and, later, in rules at the proxy.
- A program that ignores the proxy settings cannot reach the outside at
  all, because of the network policy. That fails closed, and will be met
  tool by tool.
- The proxy is one more thing that has to be up for an agent to work. It
  holds no state and restarts in seconds.
- On a cluster whose network does not enforce policies, the policy is
  silently not applied. This has to be checked on each kind of cluster.

## Alternatives considered

- **Keys in the agent's environment.** As safe as a laptop and no safer;
  kept only for the model token.
- **A purpose-built agent proxy.** Some exist and do more out of the box
  (placeholder swapping, a tool allow-list). The one that fits best has its
  Kubernetes support as a commercial product; the owner's other routing is
  already Envoy.
- **A list of allowed hosts from the start.** Safer against sending data
  out, at the cost named above. It can be added when there is a reason.
