# 0012. The memory interface is an HTTP API with a CLI; inference routines run in the harness

Status: Accepted (2026-11-18). Supersedes 0004.

## Context

ADR 0004 made memory a standalone network service reached as a remote MCP
server. The owner then set two goals: the memory must be at least as useful as
Claude Code's built-in memory (M7), and other harnesses must be able to use it
on the same level (M8). MCP ties the interface to harnesses that speak it. A
shell is the one thing every coding harness has.

## Decision

- Memory stays a standalone network service, shared by workers and the laptop.
  That part of 0004 stands.
- The service speaks plain HTTP with JSON. `cmem`, a single Go binary, is both
  the service (`cmem serve`) and its command-line client.
- Mechanical operations (store, search, show) are CLI commands.
- Operations that need inference (brief me for this task, consolidate) ship as
  prompts plus the `cmem` calls they need. The harness runs them in its own
  subagent. The service still calls no model (0006).
- An MCP adapter over the same API can be added if a harness needs one.

## Consequences

- A harness integrates through three touchpoints: inject memory at session
  start, tell the model the CLI exists, record the episode at the end.
- In Claude Code the CLI runs through the shell tool, so its commands need to
  be allowed in the permission settings.
- Each role has its own bearer token. Tailnet ACLs decide who can reach the
  service at all.
