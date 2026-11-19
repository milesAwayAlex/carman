# carman: design

Working name, taken from this directory. Last updated 2026-11-18.

This document describes the system as currently intended. It is a living
document: when the design changes, edit it to describe the new state. What
"done" means lives in `REQUIREMENTS.md`, which only humans edit. The reasons
behind decisions live in `docs/adr/` and are never rewritten, only superseded.

Parts marked **Built** exist and are tested. Parts marked **Proposed** are
recommended and not yet confirmed by the owner. Parts marked **Open** need
discussion or a test.

## 1. What this is

An event-driven agent system that picks up tasks (tickets, webhooks, scheduled
jobs), works each one to a verifiable finish without a human at a terminal, and
learns across tasks through one shared long-term memory. The same memory is
also available to the owner's interactive Claude Code sessions on their laptop.

The memory is being built first. It has to be at least as useful as Claude
Code's built-in memory (M7) and usable from any harness that has a shell (M8).

## 2. Requirements

The requirements are in `REQUIREMENTS.md`: R1 to R10 for the system and M1 to
M8 for memory, each with a "Done when" check. That file is human-owned (ADR
0009). This document refers to requirements by ID and does not restate them.

## 3. Constraints

| Constraint | Detail |
|---|---|
| Inference | Claude Agent SDK, running on the owner's infrastructure (ADR 0001) |
| Billing | The owner's Claude subscription (ADR 0002) |
| Platform | GCP. Kubernetes preferred; a VM is acceptable (ADR 0003) |
| Identity | GCP workload identity with IAM bindings |
| Language | Go for what we build (ADR 0007) |
| Memory storage | Markdown notes in a git repository (ADR 0015) |
| Network | Tailscale connects the laptop and the cluster |

Notes on billing:

- Anthropic's legal page steers "products or services" built on the Agent SDK
  toward API keys, and says plan limits assume "ordinary, individual usage".
  The owner has made the call; this note records the risk.
- The credential is a token from `claude setup-token`, mounted as
  `CLAUDE_CODE_OAUTH_TOKEN`. `ANTHROPIC_API_KEY` must not be set in the pod:
  headless mode uses a key whenever one is present.
- Any component that calls a model on its own would need a separate provider.
  This is one reason the memory service calls no model (ADR 0006).
- The subscription has a monthly spend limit per model, and background agents
  have hit it during this project. Pacing (section 5) has to account for it.

## 4. Architecture

```
tickets, webhooks ─┐
                   ├─> dispatcher ──> queue ──> worker pod (one per task)
clock ─────────────┘       │                        │
                           │                        ├─ Agent SDK session
                    task ledger                     ├─ verifier
                    pacing                          └─ end-of-task record
                           │                        │
                           └──────── memory service (HTTP, cmem CLI) ──── laptop
                                            │                          (Claude Code,
                                    markdown library in git             via Tailscale)
                                            ▲
                                     dreamer (scheduled)
```

| Component | Responsibility | State |
|---|---|---|
| **Memory service** | Section 6 | Built |
| **Dispatcher** | Turns events into task records with a done-criterion and a budget; routes later events to the task waiting for them; decides what runs now | Proposed |
| **Task ledger** | One row per task: trigger, state, what it is waiting for, session ID, branch, cost, verifier result, handoff note | Proposed |
| **Worker** | One pod per task. Runs the agent, then the verifier, then records the episode | Proposed |
| **Verifier** | A check separate from the agent's own claim. On failure the task continues with the result | Proposed |
| **Dreamer** | Scheduled agent run that consolidates memory | Proposed |

### Worker details (proposed)

- **Permissions.** `dontAsk` mode with an allowlist is the documented headless
  setting. The agent's shell inherits the pod's identity, so the IAM bindings
  on the service account are the real permission system.
- **Worker language.** The Agent SDK ships for TypeScript and Python only.
  Either the worker entrypoint is a thin TS or Python program, or Go drives the
  `claude` CLI headless. Open.
- **Built-in memory.** Turned off in workers (`autoMemoryEnabled: false`,
  `settingSources: []`) so there is one memory.
- **Sizing.** The docs suggest starting at 1 CPU, 1 GiB RAM, 5 GiB disk.

### Long-running tasks (proposed)

Tasks will sometimes wait hours on CI or a person. Start without transcript
resume: before stopping, the agent writes a handoff note to the ledger; each
wake is a fresh session given the note, the task's branch and the new event.
Add the SDK's `SessionStore` resume later for short waits. A cold resume
re-reads the whole transcript at full price, because the prompt cache has
expired by then.

## 5. Pacing (proposed)

- The service draws from the same usage windows as the owner's interactive
  Claude Code. The dispatcher reserves headroom for the owner.
- Signal: the SDK's rate-limit events carry utilization, window type and reset
  time. Confirmed for the Python SDK only.
- Policy: urgent tasks always run. Backlog tasks run only while utilization is
  under a curve that reaches full at reset. The dreamer and benchmark runs are
  backlog jobs.
- Per run: `maxBudgetUsd` and `maxTurns` are hard stops.

## 6. Memory

### The library (built)

Memory is a library of markdown notes in a git repository (ADR 0015):

```
entries/2026/11/20261118-192734-teo2.md    one thing worth knowing
episodes/2026/11/20261118-192730-a1b2.md   the raw record of a task, or the evidence for a note
flags/2026/11/20261118-193000-zz9q.md      a report that an entry is wrong
retirements/…                              an entry withdrawn by consolidation
runs/…                                     one consolidation run and its purpose
reverts/…                                  the undoing of a run
```

Each note is a YAML header and a markdown body, readable in any editor. A
generated footer of `[[links]]` to the notes it cites makes the library
browsable in Obsidian or with Markdown Oxide.

```markdown
---
id: 20261118-192734-teo2
type: entry
created: 2026-11-18T19:27:34.164125Z
scope:
    - repo:infra
kind: pitfall
created_by: agent
sources:
    - human: alex
---

Hard topology spread constraints leave pods pending because the cluster
autoscaler ignores pod topology; keep spread constraints soft.
```

How the requirements are carried:

- **Notes are written once (M2).** The service creates files and never changes
  or removes one. An entry changes by a new entry naming it in `supersedes`
  (or `absorbs`, when several are merged), or by a retirement note. What is
  current, when an entry stopped being current, its status and its flags are
  all worked out from the notes when read; none is stored.
- **Consolidation is undone by adding a note (M4).** Everything a run writes
  carries the run's ID. A revert note naming the run makes all of it stop
  counting: what the run added is no longer current, and what it replaced or
  retired is current again. Nothing is deleted, and "as of" a time while the
  run stood still returns what was current then (ADR 0018).
- **One clock (ADR 0011).** The service stamps every note; callers cannot set
  times. "As of" a past time returns what the memory held as current then.
- **Every write is one git commit.** The history is the audit trail and a
  remote is the backup.
- **Sources and status (M3).** An entry cites episodes or a named human. It is
  active when a human or a passed episode backs it, otherwise unverified.
- **No secrets (M6).** A write that looks like it holds a credential is
  refused, and the refusal is logged without the secret.
- **Hand edits.** People can add notes by hand. Changing or deleting a note by
  hand is not prevented, but `cmem check` reports it from git.

The service loads the whole library into memory when it starts and serializes
writes, so there is one service instance per library. Notes added by hand are
picked up at the next start.

### Interface (built)

The service speaks plain HTTP with JSON, and `cmem` is its command-line client
(ADR 0012). Any harness with a shell can use the memory. `cmem help` lists the
commands.

The binary is a relay (ADR 0024): it sends its command line to the service,
which runs the command and answers with what to print. What a command prints,
its flags, the session index and the hook's behaviour therefore change with a
deploy of the service; machines that use the memory do not need updating. The
binary keeps only what its own machine must do, such as working out which
repository a session is in.

| Operation | CLI | Roles |
|---|---|---|
| Find entries that apply to the session (`--everywhere`: any) | `cmem recall WORDS...` | all |
| Show an entry or episode | `cmem get ID` | all |
| Read a long entry: a page or a stretch, the parts that match, a map | `cmem get --from N [--to M] ID`, `cmem get --find WORDS ID`, `cmem get --outline ID` | all |
| Remember one thing mid-task | `cmem note --kind K (--human NAME \| --evidence E) TEXT` | laptop, worker |
| Correct or update an entry | `cmem note ... --replaces ID TEXT` | laptop, worker |
| Correct one thing in a long entry, keeping the entry | `cmem note ... --amends ID TEXT` | laptop, worker |
| Report an entry as wrong | `cmem flag ID REASON` | laptop, worker |
| Record a finished task | `cmem record --outcome O < body` | laptop, worker |
| The list for a session's start | `cmem index` | laptop, worker |
| Entries close to a moment's text | `cmem flashback TEXT` | laptop, worker |
| Open a consolidation run | `cmem consolidate start PURPOSE` | dreamer |
| Read every current entry with sources and flags | `cmem consolidate entries` | all |
| Add a curated entry | `cmem add --run RUN ...` | dreamer |
| Replace or merge entries, whatever backs them | `cmem supersede ID --run RUN [--absorb ID] [--from ID] ...` | dreamer |
| Withdraw an entry | `cmem retire ID --run RUN REASON` | dreamer |
| Read what runs changed | `cmem consolidate log [RUN]` | all |
| Undo a whole run | `cmem consolidate revert RUN REASON` | dreamer |
| Check the library for hand edits | `cmem check` | run on the service's machine |

Rules the service enforces:

- No route edits or deletes a record.
- A caller's role comes from its bearer token, and the origin stamped on what
  it writes comes from the role, never from the request.
- A note must say where it came from: what the agent observed, or the person
  who said it. Evidence is stored as an unverified episode and the entry cites
  it (ADR 0014). What a person said is trusted.
- Only the laptop role may cite a named human as a source.
- A working agent may replace an entry only with a note that is at least as
  well backed. Its own observation cannot replace what a person said or a
  verified task showed; it flags that entry instead (ADR 0017).
- A note close to an entry memory already holds is not stored until the agent
  says whether it replaces that entry or is a separate thing (ADR 0017).
- A note that amends an entry is stored beside it, and the entry is flagged
  with a pointer to the note. A short note may not replace a long entry
  unless it says it stands for the whole (ADR 0025, proposed).
- A search made in a session looks at what applies there. When another
  repository's entry matches better than anything found, the answer names
  that repository and how to search it, and shows none of its text.
- The consolidation role can add, supersede (which includes merging) and
  retire, and nothing else. Every change names a run. A run is capped at 20
  changed entries, its changes can be read back, and one revert undoes all of
  them (M4, ADR 0018).
- The consolidation role cannot cite a person. A rewritten or merged entry
  inherits the sources of the entries it names, copied by the service.
- In the Claude Code adapter a new note must say who it is for, `--here` or
  `--global`; there is no default (ADR 0023).
- A repository's scope tag comes from its origin remote
  (`repo:github.com/owner/name`), so it is the same in every clone and
  worktree whatever the directory is called (ADR 0022).
- A replacement or merge keeps the scope of what it replaces. Entries of
  different scopes are not merged until the caller names the result's scope,
  and making an entry apply everywhere takes an explicit `--global`
  (ADR 0021).

Search is in-process: text is split into word stems, and entries are ranked
with BM25. An entry matches when it shares any stem with the query.

### Getting memory to the agent without being asked (built, thresholds proposed)

A memory the model must remember to query is weaker than Claude Code's
built-in memory, whose index is loaded into every session. Two mechanisms close
that gap (ADR 0016):

- **Index at session start.** `cmem index` returns the current entries for the
  session's scope, trusted and newest first, cut to a character budget. An
  entry costs at most the 240 characters a list shows of it. The service's
  ceiling is 24,000 characters (ADR 0017), but Claude Code keeps only 10,000
  characters per hook, so its hook cuts the index to fit and can deliver it
  in several parts (ADR 0020). One part holds about 70 lessons.
- **Flashback.** At key moments a hook passes the moment's text to
  `cmem flashback`, which returns only entries that overlap it closely, at
  most three, never the same entry twice in a session. It prints nothing when
  nothing is close, so most moments add nothing to the context.

`cmem hook claude-code` wires both into Claude Code:

| Hook event | What cmem does |
|---|---|
| `SessionStart` | Adds the index, within Claude Code's limit per hook, and one paragraph on how to use `cmem` |
| `UserPromptSubmit` | The brief when one is configured and entries remain unseen; otherwise flashback on the prompt |
| `PreToolUse`, shell commands | Flashback on the command. An entry that names a program the command runs counts as close |
| `PostToolUseFailure` | Flashback on the command and its error |
| `Stop` | Flashback on the agent's own reply. If memory has something close, the turn continues once so the agent can revise before the user reads it |

The `Stop` trigger exists because of a real case: an agent recommended a
practice that had already failed for its user, and only the user remembered.
The user's question shared almost no words with the lesson; the agent's
recommendation did.

The hook never breaks a session. If the service is unreachable or the input is
unreadable it prints nothing and succeeds.

Flashback uses no model. The closeness rule is word overlap: at least two
shared stems, covering at least 40% of the entry or of the text. The retrieval
set (section 7) measures that rule and is how the numbers get tuned. A small
model as a relevance filter is added only if measurement shows precision is the
limit.

Inference routines (brief me for this task, consolidate) ship as prompts plus
the `cmem` calls they need, and the harness runs them in its own subagent. The
service calls no model.

### When memory outgrows the index (ADR 0017)

Past the budget, entries are reachable only through flashback and recall,
which are the weakest parts. Three things are meant to keep that manageable:

- **Fewer stale entries.** Replacement by working agents and the question
  before a near-duplicate (built) stop a changed lesson from leaving its old
  version current.
- **Consolidation** (not built) merges and retires, and is the only thing that
  shrinks the library.
- **A brief by a cheap model** (built, not yet measured; ADR 0020) as the
  stopgap: on a new prompt the hook hands the entries the session has not seen
  to a command the harness supplies, which asks a small model, and `cmem`
  prints the entries it names, as written.

### Still to build

- Listing episodes for the dreamer.
- Picking up hand-added notes without a restart.
- Adapters for a second harness (M8).

### Roles around the store

| Role | When | What it does |
|---|---|---|
| Recorder | After every task | `cmem record` with the verifier result |
| Dreamer | Scheduled, when capacity is spare | Reads new episodes; proposes entries and supersessions |
| Daydreamer | Before selected tasks | Returns entries with IDs and sources, not a paraphrase |

### Serving

- One Go binary, `cmem`: `cmem serve` is the service and the other commands are
  the client.
- It runs on Kubernetes as one StatefulSet with one volume holding the
  library, deployed by `scripts/deploy.sh CONTEXT` (ADR 0019,
  `deploy/README.md`). Today that is the local cluster, where the laptop
  reaches it at `http://127.0.0.1:8377` and pods at
  `http://cmem.carman.svc:8377`.
- On GCP, workers reach it inside the cluster and the laptop over Tailscale,
  with tailnet ACLs controlling reachability (not built).
- One token per role, generated at the first deploy and kept in a Secret.
- There is no automatic backup yet; a git remote for the library is the plan
  (Q6). Until then `scripts/deploy.sh CONTEXT backup FILE` copies the library
  out, and `deploy/RECOVERY.md` has the steps for starting, checking and
  restoring.
- The service recovers by itself from a stop in the middle of a write: it
  clears git's stale locks and commits whole notes left uncommitted, and
  writes are flushed to disk before they count.
- For trying things out, `scripts/dev-service.sh` runs a separate service on
  port 8378 with a library in `.local/library`.

## 7. Knowing it works

### The system (proposed)

- **Telemetry.** The CLI bundled with the SDK exports OpenTelemetry metrics,
  logs and (beta) traces via environment variables.
- **Ledger metrics.** Cost per completed task, completion rate, how often a
  human stepped in.

### Benchmarking memory (M7, M8)

Three pieces, the first two built (ADR 0013, and `bench/transfer/RESULTS.md`
for what the second has found):

- **Retrieval set (built).** `bench/retrieval/set.json` is a fixed library with
  search queries and session moments whose right answers are known.
  `scripts/bench-retrieval.sh` scores recall and flashback against it, with no
  inference, and reports how many characters flashback added to the context.
- **Transfer benchmark (built).** `bench/transfer` asks the question M7 turns
  on in its smallest form: is a lesson learned by correction in one session
  used in a later one, in the same repository and in another? Each case is a
  lesson that cannot be known without memory, graded by a regular expression
  on the file the agent writes. Arms: no memory, the lesson pre-written,
  built-in memory per repository and in one shared directory, cmem by hand,
  and cmem with the hook. It also has lessons that change, tasks no lesson
  should affect, and a test of the consolidation routine on a messy library.
  A case costs a few cents on Sonnet, so every arm can be run several times
  and every transcript read.
- **Task stream (not built).** A fixed sequence of tasks, a fresh session per
  task, only the memory carried over, each task graded by a script. Arms with
  the same model and harness: no memory, Claude Code's built-in memory, carman
  with recall only, carman with the hook. The score is the gain over the
  no-memory arm, with tokens added by memory reported alongside.

The plan for the task stream was to adapt Continual Learning Bench, which
already runs Claude Code headless in a container with a fresh session per
instance. Its first pilot here was inconclusive: the task was noisy, and the
arms were confounded by differently worded instructions (since fixed). The
transfer benchmark is what decisions rest on for now. A stream of our own
would cover:

- a fact learned early and needed later;
- a fact that changes midway;
- a pitfall hit once;
- the tempting wrong answer: a later, different-looking design task where a
  practice that failed here earlier is the obvious recommendation, graded by
  checking the manifest the agent produces;
- a wrong memory that should not be followed;
- a question memory cannot answer.

M8 is checked by running the carman arm on a second harness with only the CLI.

## 8. Open questions

| # | Question |
|---|---|
| Q1 | Embeddings: only if the retrieval set shows word search is the limit. Evidence so far says the quality of what is written matters more than ranking |
| Q2 | Worker language: thin TS/Python on the SDK, or Go driving the CLI? |
| Q3 | Which tasks get a daydreamer pass, and on which model? |
| Q4 | Human approval gates (R9) |
| Q5 | Turn off built-in Claude Code memory on the laptop once the hook is installed? |
| Q6 | Where the library's git remote lives, and who may push to it |
| Q7 | Should the laptop record episodes automatically, or only write notes? |
| Q8 | Answered by ADR 0023: a new note says `--here` or `--global`. Still open within it: a level for one organisation's repositories, between a single repository and everywhere |

To test early, because the docs do not say:

- What happens when a usage window runs out mid-run.
- Whether the TypeScript SDK emits the same rate-limit events as Python.
- Whether a session can be resumed after an SDK upgrade.

## 9. Decisions

Decisions are recorded one per file in `docs/adr/`. The index,
`docs/adr/README.md`, shows which are accepted and which are still proposed.

## 10. Alternatives considered

| Option | Why not (for now) |
|---|---|
| Claude Managed Agents | Hosts the loop and sandbox on Anthropic's side; has memory stores, outcomes and a "dreaming" preview built in. Rejected under ADR 0001 and 0002 |
| Postgres as the memory store | Built first, then replaced. Not readable or reviewable by a person, and one more thing to host and back up (ADR 0015) |
| Hindsight (Vectorize, MIT) | Postgres, MCP, retain/recall/reflect. Its retain and reflect call a model |
| Graphiti (Zep) | Closest temporal data model. Needs a graph database and a model call on every write |
| Basic Memory, Agentcairn | Same shape as ours (markdown as the source of truth, a rebuildable index). Neither has our verification or role rules |
| Mem0, claude-mem | Mem0's local server was withdrawn in July 2026; claude-mem is local to one machine |

## 11. Build order

1. Done: the memory library, the HTTP API, the `cmem` CLI, secret rejection,
   the session index, flashback, the Claude Code hook, the retrieval set, the
   transfer benchmark, replacement by working agents, consolidation runs with
   retire and revert (M4), the consolidation routine, and import from Claude
   Code's built-in memory.
2. Done: the service on the local cluster. Next: install the hook for the
   owner's own sessions, import the built-in memories, and start using it.
   Then a backup of the library to a git remote.
3. A memory larger than the index: the scale test, and the cheap-model brief
   if it shows the tail is not being found (ADR 0017).
4. Deploy the service to the cluster and reach it over Tailscale (R4).
5. A second harness on the benchmark (M8).
6. The task-stream benchmark (M7), if the transfer benchmark stops being
   enough to decide on.
7. Walking skeleton of the task path: one trigger, one worker, one verifier,
   `cmem record` at the end.
8. Dispatcher pacing and the task ledger's wait-and-wake routing.
9. Dreamer, once there are enough episodes and a benchmark to check it against.

## 12. References

- Agent SDK: [overview](https://code.claude.com/docs/en/agent-sdk/overview),
  [hosting](https://code.claude.com/docs/en/agent-sdk/hosting),
  [sessions](https://code.claude.com/docs/en/agent-sdk/sessions),
  [cost tracking](https://code.claude.com/docs/en/agent-sdk/cost-tracking),
  [observability](https://code.claude.com/docs/en/agent-sdk/observability),
  [secure deployment](https://code.claude.com/docs/en/agent-sdk/secure-deployment)
- Claude Code [hooks reference](https://code.claude.com/docs/en/hooks)
- Policy: [legal and compliance](https://code.claude.com/docs/en/legal-and-compliance),
  [Agent SDK with your Claude plan](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan)
- Consolidation: [Agentic Context Engineering](https://arxiv.org/html/2510.04618v2),
  [Sleep-time Compute](https://letta.com/blog/sleep-time-compute),
  [Anthropic: components of agent memory](https://anthropic.com/engineering/components-of-agent-memory)
- Benchmarks: [Continual Learning Bench](https://github.com/pgasawa/continual-learning-bench),
  [VibeMemBench](https://arxiv.org/abs/2609.23570)
- Markdown as the source of truth: [agent memory should live in git](https://www.mintmcp.com/blog/agent-memory-should-live-git),
  [Markdown Oxide](https://publish.obsidian.md/markdown-oxide/index)
