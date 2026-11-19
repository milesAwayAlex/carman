# Architecture decision records

One decision per file. Each record has a status line, the context, the
decision, and its consequences.

- **Proposed**: written down, not yet agreed by the owner.
- **Accepted**: agreed. The file is not edited again.
- **Superseded by NNNN**: replaced. Only the status line changes.

To change a decision, add a new record and mark the old one superseded.

| # | Decision | Status |
|---|---|---|
| [0001](0001-agent-sdk-on-own-infrastructure.md) | Build on the Claude Agent SDK on our own infrastructure | Accepted |
| [0002](0002-bill-to-subscription.md) | Bill inference to the owner's Claude subscription | Accepted |
| [0003](0003-kubernetes-on-gcp.md) | Run on Kubernetes on GCP with workload identity | Accepted |
| [0004](0004-memory-as-network-service.md) | Memory is a standalone network service | Superseded by 0012 |
| [0005](0005-build-the-memory-service.md) | Build the memory service rather than adopt one | Accepted |
| [0006](0006-no-model-calls-in-memory.md) | The memory service makes no model calls | Accepted |
| [0007](0007-go-and-postgres.md) | Go and Postgres | Accepted; Postgres half superseded by 0015 |
| [0008](0008-rows-with-supersession.md) | Rows with supersession, not a graph database | Superseded by 0015 |
| [0009](0009-human-owned-requirements.md) | Requirements are human-owned | Accepted |
| [0010](0010-development-conventions.md) | Development conventions for the memory service | Proposed |
| [0011](0011-one-timeline-for-entries.md) | Entries have one timeline: when the memory held them as current | Proposed |
| [0012](0012-http-api-and-cli.md) | The memory interface is an HTTP API with a CLI | Accepted |
| [0013](0013-benchmark-approach.md) | Benchmark with a retrieval set and a task stream | Proposed |
| [0014](0014-notes-carry-evidence.md) | A note carries its evidence; humans are cited only from the laptop | Proposed |
| [0015](0015-markdown-library-in-git.md) | Memory is stored as markdown notes in a git repository | Accepted |
| [0016](0016-index-and-flashback.md) | An index at session start and flashback at key moments | Proposed |
| [0017](0017-growth-replacement-and-near-duplicates.md) | As memory grows: a larger index, replacement by working agents, a question before a near-duplicate | Accepted |
| [0018](0018-consolidation-runs.md) | Consolidation works in runs that are logged, capped and reverted by a note | Accepted |
| [0019](0019-memory-service-on-kubernetes.md) | The memory service runs on Kubernetes from the start, on the local cluster first | Accepted |
| [0020](0020-hook-output-limit-and-brief.md) | In Claude Code the index fits the hook's limit, in parts; a small model can read the rest | Accepted |
| [0021](0021-scope-is-kept-on-replacement.md) | A replacement keeps the scope of what it replaces; applying everywhere is said out loud | Accepted |
| [0022](0022-repository-identity.md) | A repository is named by its origin remote, not by its directory | Accepted |
| [0023](0023-a-note-says-who-it-is-for.md) | A new note says who it is for | Accepted |
