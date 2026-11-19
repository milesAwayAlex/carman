# 0007. Go and Postgres

Status: Accepted (2026-11-17). The Postgres half is superseded by 0015 (2026-11-18); Go stands.

## Context

The owner prefers Go and Postgres, and will probably self-host Postgres. The
official Go MCP SDK has a stable 1.x line with a compatibility promise.

## Decision

Write what we build in Go, starting with the memory service. Store data in
Postgres.

## Consequences

- Use the official Go MCP SDK at 1.4.1 or later; earlier versions lack HTTP
  security fixes.
- The Agent SDK has no Go version. The worker is either a thin TypeScript or
  Python program, or Go driving the `claude` CLI headless. That choice is open.
- Whether Postgres is self-hosted in the cluster or managed is open.
