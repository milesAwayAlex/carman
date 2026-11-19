# 0013. Benchmark memory with a retrieval set and a task stream on Continual Learning Bench

Status: Proposed (2026-11-18). The owner agreed to the two tiers and the arms
and said to try Continual Learning Bench. The first tier is built. The second
is blocked on two things only the owner can provide (below).

## Context

M7 and M8 are defined by a benchmark. Two existing benchmarks were assessed.

**Continual Learning Bench** (Snorkel and Berkeley, arXiv 2606.05661,
Apache-2.0). Six tasks of ordered instances, about 301 in all, one of them
coding (19 SWE-bench bug fixes). Grading is scripted. The metric is gain over
the same system run stateless. It ships a Claude Code adapter that runs
`claude -p` in a container, with a mode that starts a fresh session per
instance while a memory directory persists. Anthropic used that protocol for
its own memory evaluation.

**VibeMemBench** (Alibaba DAMO, arXiv 2609.23570, Apache-2.0). Memory is
retrieved once before each run and the agent never writes memory, so it cannot
compare our arms. Its finding matters for design: in 11 of 12 pairings a memory
system did no better than none, and 69% of failures came from noisy or
overgeneralised stored records against 1.3% from retrieval ranking.

## Decision

- **Tier 1, retrieval set (built).** `bench/retrieval/set.json`, scored by
  `scripts/bench-retrieval.sh`. No inference. It measures search, and
  flashback's precision, recall and characters added to the context.
- **Tier 2, task stream.** Adapt Continual Learning Bench. Arms, with the same
  model and harness, a fresh session per instance and only memory carried over:
  1. no memory;
  2. Claude Code's built-in memory;
  3. carman with `cmem` available but no hook;
  4. carman with the hook (index and flashback).
- Report tokens added by memory beside the score, because context bloat is the
  owner's main concern about arm 4.
- Add our own coding stream as a task inside its harness (the cases are listed
  in DESIGN.md, section 7), because 19 coding instances are too few.
- M8 is checked by running a carman arm on a second harness.

## What setting it up on the owner's machine showed (2026-11-18)

Verified by running, without calling a model:

- The benchmark installs and its own checks pass (597 of 598 tests).
- It starts containers with the `docker` command, and on this machine `docker`
  is `nerdctl` inside a Lima virtual machine. The adapter's commands work.
- A container reaches a service on the host at `host.lima.internal`.
- The Claude Code image builds and runs.

What has to change in an adapter of our own:

- **Workspace location.** The virtual machine only sees
  one shared directory. The benchmark's default workspace is elsewhere
  and silently becomes an empty directory inside the VM. The workspace has to
  move under `code-shared`, which is outside this project.
- **Credentials.** The adapter requires `ANTHROPIC_API_KEY`. Ours must pass
  `CLAUDE_CODE_OAUTH_TOKEN` instead (ADR 0002).
- **Session hygiene.** In fresh-session mode the adapter keeps the whole
  workspace between instances, including old transcripts, and prepends the last
  instance's feedback to the next prompt. Our arms clear transcripts and
  scratch files so that only memory carries over.
- **Arm 2** relies on Claude Code's auto-memory directory, which the adapter
  already uses. Arms 1, 3 and 4 turn auto-memory off.
- **Arms 3 and 4** need an image with a Linux `cmem` in it, and one `cmem serve`
  per rollout on an empty library so runs do not share memory.
- Two of the six tasks use a container flag `nerdctl` rejects and need a small
  patch. The poker, spectrum and cohort tasks need nothing extra.

## Blocked on the owner

1. Agreement to put benchmark workspaces under the VM's shared directory
   (for example `code-shared/carman-bench/`).
2. A subscription token from `claude setup-token`, exported as
   `CLAUDE_CODE_OAUTH_TOKEN` where the benchmark runs.

## Consequences

- The smallest proof that all arms work is about 8 to 12 short agent sessions.
  A full run is hundreds, draws on the subscription, and has to be paced: the
  monthly spend limit for the main model was hit during this project.
- The benchmark's default of 12 parallel workers is too many for an 8 GiB VM;
  use 1 to 3.
- Nothing in the benchmark needs Kubernetes. If that changes, it uses the local
  cluster by naming its context (`kubectl --context devvm`); the machine's
  default context is an unrelated cluster.
- Not yet confirmed: that subscription-token auth and auto-memory behave as
  expected under `claude -p` inside the container.

## What happened since (2026-11-18)

Both blockers were cleared and the four arms ran on Continual Learning Bench.
The pilot was inconclusive. The task chosen was noisy, the arms' instructions
were worded differently, which showed as a difference before any memory
existed, and parallel runs failed part of the way through. The instructions
now share one frame and the runner refuses concurrent runs.

Rather than spend more there, a much smaller benchmark was built:
`bench/transfer`. Each case teaches one lesson by a correction and probes for
it in a fresh session, in the same repository and in another. A case costs a
few cents on Sonnet, so each arm can be run several times and every transcript
read. Its results are in `bench/transfer/RESULTS.md` and its place in the plan
is in DESIGN.md, section 7. The decision above stands as the plan for a full
task stream, to be picked up if the small benchmark stops being enough to
decide on.
