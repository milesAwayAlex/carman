# 0035: A credential that does not last is renewed beside the proxy

Status: Proposed (2026-11-20). Built; running on the local cluster, waiting
for its first sign-in.

## Context

ADR 0031 has the egress proxy hold each credential as a fixed value from a
secret. That fits a service with an API key. The owner wanted agents to
reach a service that has none: its only credential is a token that lasts
an hour, obtained from a sign-in a person makes, renewed with a second
token that can be used once and replaced each time, the whole sign-in
ending after some weeks. Reading that service's code confirmed there is
nothing longer-lived to ask for.

The owner had allowed for this ("custom proxies if nothing else works") and
chose it over changing the service. Two things a fixed value cannot do:
change every hour, and be renewed by something that keeps a sign-in.

## Decision

- **`egress.renewals` names, for a credentialed host's key, a command that
  prints the header value to use.** It is run again and again in a
  container beside the proxy, in the same pod, from the agent image.
- **The proxy reads that credential from a file and reads it again when the
  file is replaced.** The container writes the new value and moves it into
  place in one step. No restart, no cluster API, and the value is never in
  a secret or a log.
- **What the command must keep, such as a sign-in, lives in a volume only
  that container mounts.** No agent pod can reach it: agents reach the
  proxy's port and nothing else in that pod.
- **Never two proxies at once when there are renewals**: a sign-in that is
  good for one holder would be spent twice.
- **The first sign-in is a person's**, made in that container by hand. So
  is the next one when the service ends the sign-in.
- **A sign-in by OAuth is kept by a command the image has**
  (`oauth-credential`), for services that have no key and no tool of their
  own, such as remote MCP servers. It learns from the service how to sign
  in, registers itself as a client, and renews with the token the service
  gives for that, which is used once and replaced each time. The person
  signs in from their own browser with `scripts/deploy.sh CONTEXT signin
  KEY URL`; the browser's way back is a port forwarded to the renewal, so
  what the sign-in yields is only ever in that container's volume.
- Fixed credentials are untouched: they are still read once, from the
  secret, as before.

## Consequences

- An agent acts at that service as the person who signed in, with
  everything that person may do there. The owner wants that for this
  service; it is a reason to prefer a service's own narrower credential
  where one exists.
- When the sign-in ends, or the command fails, the proxy keeps the last
  value until it expires and agents then get the service's refusal. The
  container says so in its log; nothing tells the owner yet.
- The renewal uses the agent image because the tool that renews is
  whatever the installation put there; the image is large for the job.
- The proxy pod now has a volume, so it starts only where that volume is.
