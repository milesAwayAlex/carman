# 0004. Memory is a standalone network service

Status: Superseded by 0012 (2026-11-18)

## Context

The owner wants one long-term memory, independent of inference, shared by the
cloud workers and by their own Claude Code sessions on a laptop. The laptop and
the cluster are connected by Tailscale.

## Decision

Memory is its own service, reached over the network as a remote MCP server.
Workers reach it inside the cluster; the laptop reaches it over Tailscale.

## Consequences

- Tailnet ACLs decide who can reach the service. Each role (laptop, worker,
  consolidation) gets its own credential, because the roles have different
  permissions.
- Claude Code's built-in memory should be turned off in workers so there is one
  memory. Whether to do the same on the laptop is open.
- Memory is the one component that is not disposable, so its database needs
  backups.
