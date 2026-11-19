# 0001. Build on the Claude Agent SDK on our own infrastructure

Status: Accepted (2026-11-17)

## Context

There are three ways to build an agent on Claude. Managed Agents has Anthropic
run the agent loop and host a sandbox per session. The Claude Agent SDK is the
Claude Code harness as a library, hosted by us. The Messages API with tool use
leaves the loop and everything else to us.

The owner is a long-time Claude Code user and wants that behaviour running on
infrastructure they control.

## Decision

Use the Claude Agent SDK, hosted on our own infrastructure.

## Consequences

- We host the sandbox, session persistence and scheduling.
- Managed Agents features have no equivalent here and must be built: checking
  outcomes against a rubric (our verifier), memory stores (our memory service),
  and consolidation (our dreamer).
- The SDK ships for TypeScript and Python only, which leaves the worker's
  language open (see DESIGN.md, open questions).
