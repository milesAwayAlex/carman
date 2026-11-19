# 0002. Bill inference to the owner's Claude subscription

Status: Accepted (2026-11-17)

## Context

Inference for this system has to be billed to the owner's Claude
subscription: the system is treated as an extension of the owner's own
machine, not as a separate product with an API account.

Anthropic's legal page steers "products or services" built on the Agent SDK
toward API keys and says plan limits assume "ordinary, individual usage".
Nothing found addresses an internal service running on one person's seat.

## Decision

Workers authenticate with the owner's subscription, using a token from
`claude setup-token` mounted as `CLAUDE_CODE_OAUTH_TOKEN`.

## Consequences

- `ANTHROPIC_API_KEY` must not be set in worker pods; headless mode uses a key
  whenever one is present.
- The system shares usage windows with the owner's interactive Claude Code, so
  the dispatcher has to pace work and reserve headroom.
- Any component that calls a model on its own would need a separate provider.
  See 0006.
- The policy position is the owner's call. If Anthropic's guidance changes, the
  fallback is API or cloud-provider billing, which changes pacing from
  window-based to budget-based.
