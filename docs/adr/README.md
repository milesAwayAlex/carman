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
| [0010](0010-development-conventions.md) | Development conventions for the memory service | Accepted |
| [0011](0011-one-timeline-for-entries.md) | Entries have one timeline: when the memory held them as current | Accepted |
| [0012](0012-http-api-and-cli.md) | The memory interface is an HTTP API with a CLI | Accepted |
| [0013](0013-benchmark-approach.md) | Benchmark with a retrieval set and a task stream | Accepted |
| [0014](0014-notes-carry-evidence.md) | A note carries its evidence; humans are cited only from the laptop | Accepted |
| [0015](0015-markdown-library-in-git.md) | Memory is stored as markdown notes in a git repository | Accepted |
| [0016](0016-index-and-flashback.md) | An index at session start and flashback at key moments | Accepted |
| [0017](0017-growth-replacement-and-near-duplicates.md) | As memory grows: a larger index, replacement by working agents, a question before a near-duplicate | Accepted |
| [0018](0018-consolidation-runs.md) | Consolidation works in runs that are logged, capped and reverted by a note | Accepted |
| [0019](0019-memory-service-on-kubernetes.md) | The memory service runs on Kubernetes from the start, on the local cluster first | Accepted |
| [0020](0020-hook-output-limit-and-brief.md) | In Claude Code the index fits the hook's limit, in parts; a small model can read the rest | Accepted |
| [0021](0021-scope-is-kept-on-replacement.md) | A replacement keeps the scope of what it replaces; applying everywhere is said out loud | Accepted |
| [0022](0022-repository-identity.md) | A repository is named by its origin remote, not by its directory | Accepted |
| [0023](0023-a-note-says-who-it-is-for.md) | A new note says who it is for | Accepted |
| [0024](0024-the-service-runs-the-command-line.md) | The service runs the command line; the installed binary relays | Accepted |
| [0025](0025-a-note-can-correct-part-of-an-entry.md) | A note can correct part of an entry, which is kept and points at the correction | Accepted |
| [0026](0026-here-does-not-narrow-a-replacement.md) | `--here` does not change who a replacement is for | Accepted |
| [0027](0027-the-dreamer-is-the-first-agent.md) | The dreamer is the first agent, and it runs on the cluster beside the memory | Accepted |
| [0028](0028-carman-holds-its-own-credentials.md) | carman holds its own credentials, as secrets in its cluster | Accepted |
| [0029](0029-consolidation-is-checked-as-it-writes.md) | Consolidation is checked as it writes; what fails is retried with the reason, then escalated | Accepted |
| [0030](0030-carman-is-packaged-as-a-chart.md) | carman is packaged as a Helm chart | Accepted |
| [0031](0031-agents-reach-the-outside-through-a-proxy.md) | Agents reach the outside through a proxy that holds their credentials | Accepted |
| [0032](0032-what-runs-on-a-cluster-does-not-carry-the-project-name.md) | What runs on a cluster does not carry the project's name | Accepted |
| [0033](0033-on-a-shared-cluster.md) | On a shared, managed cluster: published images, secrets made in the cluster, a nightly copy to a bucket | Accepted; the nightly copy superseded by 0038; the install at each job's start, in part, by 0041 |
| [0034](0034-an-interactive-agent.md) | An agent can be interactive: a standing pod a person attaches to | Accepted |
| [0035](0035-credentials-that-do-not-last.md) | A credential that does not last is renewed beside the proxy | Accepted |
| [0036](0036-the-owners-attention.md) | An agent asks for the owner's attention with one command, and the owner can pick its conversation up | Accepted; resuming from an interactive agent superseded by 0040 |
| [0037](0037-services-and-a-namespace-of-their-own.md) | Agents reach the installation's services on the cluster, and have a namespace to run their own | Accepted |
| [0038](0038-state-is-copied-to-a-store-the-platform-gives.md) | The installation's state is copied to a store the platform gives, by a job of the chart | Accepted |
| [0039](0039-the-dispatcher-keeps-its-state-in-tables-of-its-own.md) | The dispatcher keeps its state in tables of its own, in a Postgres database it owns | Accepted |
| [0040](0040-a-conversation-is-resumed-as-its-agent.md) | A kept conversation is resumed as the agent that had it, in a pod of its own | Accepted |
| [0041](0041-a-job-starts-from-a-warm-image-made-in-the-cluster.md) | An agent's job starts from a warm image, made in the cluster and kept in the installation's registry | Accepted |
| [0042](0042-the-dispatcher-runs-as-a-service-that-starts-jobs-and-watches-every-job.md) | The dispatcher runs as a service that starts Jobs from CronJob templates and records how every Job ended | Accepted |
| [0043](0043-the-dispatcher-keeps-time-says-what-did-not-happen-and-reads-usage.md) | The dispatcher starts work on time, says what did not happen, and reads the account's usage | Accepted |
| [0044](0044-what-memory-is-not-for-what-adds-to-an-entry-and-what-the-dreamer-may-read.md) | What memory is not for; a note that adds to an entry; what the dreamer may read and check | Accepted |
| [0045](0045-messages-between-sessions-agents-and-the-owner.md) | Messages between sessions, agents and the owner are events in the dispatcher's log | Accepted |
| [0046](0046-a-restricted-routine-may-run-named-commands-that-only-read.md) | A restricted routine may run named commands that only read | Accepted |
| [0047](0047-who-may-write-to-an-agent.md) | Who may write to an agent: the owner, its own sessions, an answer, and the agents it lists | Accepted |
| [0048](0048-the-notice-channel-is-open.md) | The channel that carries notices to the owner is mostly trusted, not private: a notice says roughly what happened and how urgent, with no personal data, internal identifiers or keys | Accepted |
| [0049](0049-sessions-on-the-owners-machine-are-a-place-with-a-token-of-its-own.md) | Sessions on the owner's own machine are a place with a token of its own: they write as the owner may, are written to as ADR 0047 allows, and a message for one that is not running waits for the next of its name | Accepted |
| [0050](0050-the-model-credential-is-held-at-the-proxy-which-records-what-each-agent-used.md) | The model's credential is held at the egress proxy, which knows each caller by its pod's address and records what each agent used | Accepted |
| [0051](0051-the-owners-token-stays-on-the-owners-machine-and-the-dispatcher-knows-its-hash.md) | The owner's token stays on the owner's machine, and the dispatcher knows only its SHA-256; the owner's client runs there | Proposed |
