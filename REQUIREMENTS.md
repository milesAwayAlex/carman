# Requirements

This file defines what "done" means for carman. It is owned by humans.

- Agents read it and build against it. Agents never edit it.
- A requirement is approved when a human commits it. This first version was
  drafted by an agent on 2026-11-17 from the design discussion; the owner's
  commit of it is the approval.
- IDs are permanent. A retired requirement keeps its ID and is marked retired.
- If a requirement looks wrong, missing or impossible to check, an agent says so
  and proposes wording. A human decides and makes the edit.
- "Done when" is the check. A requirement is met only when that check can be
  shown to pass.

## System

| ID  | Requirement                                                                                                       | Done when                                                                                                                                                                                |
| --- | ----------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R1  | Tasks have measurable outcomes; done is checked, not claimed                                                      | Every task record has a done-criterion set before work starts, and a verifier result recorded separately from the agent's own report                                                     |
| R2  | Triggers are many-to-many: incoming tickets and webhooks, plus an internal clock                                  | A task can be started by a webhook and by a schedule, and a running task can receive later events addressed to it                                                                        |
| R3  | The agent works with GCP, GitHub and external services                                                            | A worker completes a task that touches each of GCP, GitHub and one external service                                                                                                      |
| R4  | One long-term memory is shared by every task and by the owner's laptop                                            | An entry written during a worker task is returned by recall on the laptop, and the reverse                                                                                               |
| R5  | Memory is a separate module, independent of inference                                                             | Agents reach memory only through its network interface, and the memory service runs with no model provider configured                                                                    |
| R6  | Worker pods, disks and workspaces are disposable                                                                  | Deleting any worker pod mid-task loses no memory, and the task can be picked up again                                                                                                    |
| R7  | The system knows its usage limits and paces itself to use the subscription fully without starving interactive use | The dispatcher records current utilization and reset time; backlog work stops before the owner's reserved headroom is used; unused capacity at reset stays under a target the owner sets |
| R8  | Memory stays useful over long stretches: it is consolidated and actively retrieved                                | On the replay set, tasks run with memory do at least as well as without it, and consolidated memory does at least as well as raw episodes                                                |
| R9  | Human approval gates                                                                                              | Open. To be written after observing unattended runs                                                                                                                                      |
| R10 | The owner can tell whether the system is working                                                                  | For any period the owner can see cost per completed task, completion rate, and the number of human interventions                                                                         |

## Memory

| ID | Requirement                                                              | Done when                                                                                                                                                                                                    |
| -- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| M1 | The memory service calls no model                                        | The service has no model client or provider configuration, and its tests pass with no network access beyond its database                                                                                     |
| M2 | Raw records are append-only; curated entries change only by supersession | No interface operation edits or deletes an episode. Changing an entry creates a new entry that supersedes it; the old one stays retrievable, and a recall "as of" a past date returns what was current then  |
| M3 | Every entry says where it came from and whether that source was verified | Every entry cites at least one episode or a named human. Recall results show each entry's sources and their verification outcome. Entries backed only by failed or unverified episodes are marked unverified |
| M4 | Consolidation makes small, reversible changes                            | The consolidation role can only add, supersede and retire entries, using its own credential. Each run's changes are logged, capped in size, and can be reverted as a unit                                    |
| M5 | Memory is retrieved both before a task and during it                     | An agent can call recall at any point in a task. A pre-task brief returns entries with their IDs and sources, not a paraphrase                                                                               |
| M6 | No secrets in memory                                                     | A write containing a credential-shaped string is rejected, and the rejection is logged without the secret                                                                                                    |
| M7 | Memory must be useful                                                    | On the benchmark task stream, with the same model and harness, tasks run with carman succeed at least as often as with Claude Code's built-in memory, at a cost per task within a margin the owner sets      |
| M8 | Memory is modular                                                        | A harness needs only a shell and the CLI to use memory fully. Done when a second harness completes the benchmark that way and scores within the same margin                                                  |

## Proof of concept

The first build is the memory service, built by agents against M1 to M6. It
proves two things: that the memory design holds, and that agents can build from
human-approved requirements.

In scope: episodes and entries; recall, get, note and flag; replacing an entry
with a note that is at least as well backed; the end-of-task record endpoint;
unrestricted supersede, retire and revert behind a separate credential;
full-text search; a connection from the owner's laptop over Tailscale.

Out of scope for now: the consolidation agent itself, the pre-task brief (the
second half of M5), embeddings, the dispatcher and workers (R1 to R3, R6 to
R10).
