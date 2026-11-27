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
- The credential is a token from `claude setup-token`. Agent pods have
  `CLAUDE_CODE_OAUTH_TOKEN` set to a placeholder, and the egress proxy's
  meter puts the token on each request to the model (ADR 0050;
  `agent.modelCredential: proxy`; `pod` puts it back in their environment).
  `ANTHROPIC_API_KEY` must not be set in the pod: headless mode uses a key
  whenever one is present.
- Any component that calls a model on its own would need a separate provider.
  This is one reason the memory service calls no model (ADR 0006).
- The subscription has a monthly spend limit per model, and background agents
  have hit it during this project. Pacing (section 5) has to account for it.

## 4. Architecture

```
tickets, webhooks ─┐
                   ├─> dispatcher ───────────> worker pod (one per task)
clock ─────────────┘       │                        │
                           │                        ├─ Agent SDK session
              event log, tasks (Postgres)           ├─ verifier
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
| **Dispatcher** | Keeps a durable log of events; turns events into tasks; starts work, routes later events to the task waiting for them, and records what did not happen (an overrun, a missed deadline or tick); decides what runs now. Its state is in tables of its own in a Postgres database it owns (ADR 0039). It runs as a service on the cluster: it starts a task as a Job made from the job template of the CronJob its class names, and records the end of every Job in its namespace, whoever started it (ADR 0042). It starts work on schedules by time of day from the values, expects a Job at each tick of the CronJobs it does not start and records a tick without one, tells the owner of what failed or did not happen through a task of class `notify` (withheld until switched on), and keeps readings of the account's usage, for each model its agents use, and each routine run's cost (ADR 0043, 0052). It carries messages between sessions, agents and the owner as events in its log, delivered into a running session by the harness's hooks and starting a session that is not running again as its agent (ADR 0045) | Built and running on the local cluster (`cmd/dispatcher`, `internal/dispatch`); schedules the usage reading, and the dreamer, the backup, the warm job and agents where their values say (ADR 0059); notices withheld; admission rule built, off |
| **Task ledger** | One row per task: trigger, state, what it is waiting for, session ID, branch, cost, verifier result, handoff note. State, waits and attempts are the dispatcher's tasks table; the rest is to come | Partly built, in the dispatcher |
| **Worker** | One pod per task. Runs the agent, then the verifier, then records the episode | Proposed |
| **Verifier** | A check separate from the agent's own claim. On failure the task continues with the result | Proposed |
| **Dreamer** | Scheduled agent run that consolidates memory | Built, switched off (ADR 0027) |

### Messages (ADR 0045)

A session (a conversation, wherever it runs or does not), an agent by name
and the owner can be written to with the dispatcher's client
(`dispatcher peers|send|reply|inbox|read|message|watch`). A message and
what became of it are events in the dispatcher's log. An agent's pod
proves which agent it is with a token the cluster makes for it for the
dispatcher; which session of its pod it is, is its word. That token lasts
an hour and the cluster replaces its file before then, so the client reads
the file at each call: the waiter and the watcher run for days. A running session
gets messages through the harness's hooks: with the next tool results, at
the end of its turn, or, idle, woken by an `asyncRewake` waiter that holds
a long poll to the dispatcher (channels, the harness's own way, are not
available to the agents' credential); the waiter's `rewakeMessage` and
`rewakeSummary` make the woken session read it as a message, where the
harness would otherwise call it a hook's blocking error. A message for a session that is not
running becomes a task that resumes it as its agent, headless, under
ADR 0040's one-writer lock, and its last words go back as the answer; one
for an agent by name starts a new conversation; one for the owner is a
notice. A sender's new messages (not answers; not the owner's) start at
most `dispatcher.messages.runsPerSender` runs in `runsPeriod` (24 in three
hours by default); past it a message is held, the sender told why, until
the owner lifts it (`dispatch lift SENDER`), which starts what was held.
The owner writes with `scripts/deploy.sh CONTEXT dispatch send`,
which runs the client on the owner's machine with the owner's token; the
token is kept only there, and the dispatcher knows its SHA-256 (ADR 0051).
A notice's words are checked before they are handed on: one that looks
like a credential or matches a shape the installation names
(`dispatcher.notices.withhold`) is replaced by one saying where to read it,
recorded as `notice.replaced`; words without a shape are not caught.
(2026-11-25) Such a run has its agent's time limit (the Job's deadline,
plus ten minutes; thirty minutes where there is none) and records its cost
as `run.usage`, as a routine's run does.

Who may write to an agent and its sessions (ADR 0047): the
owner, the agent's own sessions, a session answering a message it was
sent, and the agents its `writtenBy` lists; nobody else. With no list,
only the owner and those cases. A message that is not allowed is refused
at sending, with who may, and recorded as `message.refused`; `peers` marks
what the caller may not write to.

`peers` lists who is worth writing to (2026-11-27). The agents a message
does something for: those the cluster defines with a job (a message
starts a run), and interactive ones, defined by a standing pod that
carries the label `agent` or, for one the cluster no longer defines,
while a session of it that runs is interactive (a message waits for one
of its sessions). Not the place, which is no agent, and not an agent
whose runs are headless and that has no job a message starts (the
hourly usage reading). Then the sessions seen within `--since`: every
one that runs, first, then by when last seen; of those that do not, only
the latest of each agent and name, or one a message was exchanged with,
and only of an agent the cluster defines or of the place. `--all` lists
every session seen. A session that its name reaches is shown as
`AGENT/NAME` or `PLACE/NAME`, the address that outlives a restart, with
its id shortened beside it; `session/` and the first eight characters of
an id resolve too. The API's `/v1/peers` (`?all=yes`) adds `named` and
`running` to each peer.

Sessions on the owner's own machine (ADR 0049) are a place with
a token of its own (the dispatcher's `host` role; the place is
`dispatcher.host.place`, `host` by default), outside the cluster, with no
pod. Each is `PLACE/NAME`, named for the directory it runs in unless
`DISPATCHER_SESSION_NAME` says otherwise; which of the place's sessions a
caller is, is its word. They write to any agent and session, as the owner
may, but are not the owner: a message from one to `owner` is a notice, and
the owner's inbox is not theirs. Agents write to them as ADR 0047 allows
(`dispatcher.host.writtenBy`). The same hooks carry messages into them,
with `DISPATCHER_URL`, the host token and `DISPATCHER_PLACE` from the
environment. One runs while its waiter listens or a hook was heard within
a day. One that is not running is not started by a message: the message
waits, and the next session of its name there, or the same one resumed, is
handed it. The dispatcher's Service may be a LoadBalancer
(`dispatcher.service.type`; the local values do this, on port 8379), and
`scripts/deploy.sh CONTEXT env` writes `.local/dispatcher.env`.

The owner goes to a session with `carman go` (ADR 0057, proposed): the
program asks the dispatcher for the sessions, lists those that wait on him
first (idle with a message for him unread, then working with one, then
idle, then busy, then those not running that can be gone to), and for one
in a standing agent's pod runs `kubectl exec` there into `agent-view`, a
tmux session of its own in the pod's group that shows that session's
window, with no status line and no prefix. How the attach ended tells a
leave (status 0) from a dropped connection, after which `--keep` attaches
again. A job's kept conversation is still gone to by the checkout's
`agent resume`.

A standing agent's pod attends to its own sessions (ADR 0053, proposed).
`dispatcher attend`, started by `agent-session`, holds a session the
dispatcher believes runs in the pod to have ended when the harness process
its `SessionStart` hook recorded (pid and start time) has gone, and says so
(`/v1/attend/gone`). A message for a named session of a standing agent that
is not open waits for the pod to open it: the watcher runs `agent-open
--reopen NAME` for a name that is remembered there (a new window, or the
shell the dead harness left replaced), once, then again after a backoff at
most four times, and says on the message why it still waits
(`/v1/attend/hold`). A name not remembered is not opened. The session
opened again, or one of that name opened by hand, takes the messages of
the earlier sessions of its name that are not running. A message a wake
handed a session whose harness the watcher then finds gone, with no word
from the session in between, is recorded as not delivered and delivered
once more, marked as a second time: at least once, never silently lost.

The plan's usage limit is said to whoever waits (ADR
0067, proposed). A run started for a message that
fails with the limit's words is recorded as `message.limited`, and its
sender is told the work was not done and when the limit resets; the
dispatcher sends the message again once after the reset when the sender
asked (`--again-after-limit`) or `dispatcher.messages.againAfterLimit`
says so. The watcher reads each running session's transcript, and a last
entry that is the harness's stop at the limit is told
(`/v1/attend/limited`) to the senders of what the session took and has
not answered, and to whoever watches it, once for each stop.

### A trial's agents and the model (ADR 0065, proposed)

A trial installation's agents (ADR 0054) reach the model at a second
listener of the meter's (`egress.model.trials`, off by default), which
serves the model's API and nothing else: the proxy's own port, with its
other credentials, stays closed to the sandbox, and no credential goes
there. The caller is the address its connection comes from, looked up
among the sandbox's pods (a Role over pods there, to read): a pod that
says which trial and which agent it is, recorded as `trial/TRIAL/AGENT` in
the same `model.usage`. Only the models the installation lists for trials
pass (the cheapest unless said), and so many requests a day, all trials
together, until the proxy keeps budgets. Two network policies let pods
labelled for it out of the sandbox to that port and into the
installation's namespace at it.

### Who comes in, and a pod's first second (ADR new-who-comes-in-and-a-new-pods-first-second, proposed)

Nothing outside the installation's namespace connects to a pod in it but
by a way in that is named: a service reached through a load balancer
says who its traffic comes from (`admitFrom`: a CIDR, or another
namespace's pods by label), and labelled sandbox pods reach the meter's
port for trials. The proxy, which puts credentials on what it passes,
admits only the pods that leave through it, whether or not the namespace
is isolated. On the kind of cluster this runs on, the network's rules
reach a new pod 0.1 to 1.2 seconds after it starts: until then its own
namespace refuses it, and its own rules do not hold it, which is why the
destination's rules, not the stranger's, are what closes the
installation. Every pod that is a client waits once at its start
(`dispatcher ready`) until the dispatcher, the memory and the proxy
answer.

### A trial's own proxy and meter (ADR 0068, proposed)

A trial (ADR 0054) may have the chart's egress proxy and meter, made
from the checkout (`TRIAL_PROXY`), so that a change to either is tried in
the sandbox. Its meter sends to a stand-in for the model's API
(`dispatcher stand-in`: the API's shapes, the headers of ADR 0064, one
credential, the word "ok", or a tool asked for where the conversation's
first message scripts it, so that a real harness does what a run does with
no model), or on to the installation's meter for trials
(ADR 0065), where the trial's proxy is the caller.

### Triggers (ADR 0063, proposed)

Beside the clock, a command and a message, work starts when something
happens. A rule of the installation's (`dispatcher.rules`) names events
(a type, a subject, fields of the data that must equal a value) and a
class; the dispatcher, reading its log with a place kept in `marks`,
submits one task for an event (`rule/NAME/EVENT-ID`), at most so many in
a period (an event beyond that is dropped and recorded as `rule.held`),
and none for an event about a run a rule started, so that a rule cannot
feed itself. The run is told what triggered it in `TASK_TRIGGER_*`, and
a routine's prompt says so. What happens outside enters two ways. A
watch (`dispatcher.watches`) is a job with no model on the dispatcher's
schedule: `dispatcher source` asks a URL or runs a command, keeps what a
filter keeps and tells the dispatcher its digest; the dispatcher records
`watch.changed` when the digest differs from the last it remembers. A
hook (`dispatcher.hooks`) is a door at `/v1/hooks/NAME`, where a call is
recorded as `hook.received` only when its body is signed with the hook's
secret. Neither keeps the source's text: a value from outside is kept,
and carried to a run, only if it is a token (no spaces), so a run is
handed references and reads the source itself. Off unless written.

### The improvement loop (ADR 0058, proposed)

The platform keeps a list of what should be improved in itself, in the
dispatcher's log: an item is `improvement.noted` and the
`improvement.changed` and `improvement.briefed` events after it, folded
into its present state (noticed, agreed, building, done, declined), and
kept as long as the log is. The owner and the owner's machine read and
write it (`scripts/deploy.sh CONTEXT dispatch improve`, `dispatcher
improve`), and the agents the installation names; only the owner agrees
or declines. One agent, the surveyor, runs the routine `improve` on a
schedule: it reads the evidence since the last survey through one call
of the dispatcher (failed and missed runs, refusals, each run's cost,
what each deploy deployed (`installation.deployed`, written by the deploy
script: the commit and the subjects of the commits since the one before;
any agent reads the latest with `dispatcher deployed`, and its pod's
image is `AGENT_IMAGE`),
and messages and the part of a routine's report that says what it had no
tool for and asks the owner (`routine.report`, from `cmem routine`), with
their words only from the senders the installation lists), brings the list up to date, and asks for at most one brief. The
dispatcher decides, calling no model: the item is agreed by the owner or
of a kind and size the installation lets start by itself (none, by
default), no brief was written for it, the addressee is named, the day's
budget has room, and the plan's usage has room (pacing's `usage.room`
for every shared window, ADR 0060, or every window under a mark). A brief that passes goes as a
message vouched for by the dispatcher, which is the surveyor's only way
to another agent; with `improvement.send` off it is recorded with its
text and not sent. The owner is told in one line, at most once a day,
that items wait for his decision; with `improvement.notify` off the line
is recorded only. Nothing is merged or deployed by the loop. The surveyor
reads what others wrote, so it is held to the one command it uses,
`dispatcher improve` (ADR 0061, proposed).

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
- Signal: the harness's stream carries rate-limit events with each window's
  utilization and reset time. Built (ADR 0043, 0052): readings of each
  model the installation's definitions name (`usage.observed`, the model
  asked for one word: the cheapest every hour, the others every six hours;
  a plan can limit one model on its own, which a call to another does not
  show, and a call refused for a limit is recorded as that model's
  reading, with the limit, its reset and the harness's words) and each
  routine run's cost and last reading (`run.usage`), as events in the
  dispatcher's log kept for the period of events that belong to no task.
  The proxy's own record of every agent's model use (`model.usage`, ADR
  0050) is the count that does not depend on what an agent says. The
  meter also reads the windows from the headers of the answers it passes
  and writes them as readings of the model called, each few minutes and at
  once when a status changes or a call is refused; the job then reads
  only a model nobody called in its period (ADR
  0064, proposed).
- Where the policy acts: the dispatcher's admission, asked before each start
  with what is running; a task it holds says why.
  A task may be set after a class (ADR 0062, proposed): it waits for the
  tasks of that class submitted before it to end, however they end, at
  most a time; the backup is after the dreamer. Admission judges a task
  by the ladder of the first routine its job runs.
- Policy: urgent tasks always run. Backlog tasks run only while utilization is
  under a curve that reaches full at reset. Built first as a threshold, off
  by default (ADR 0043): below a priority, a task is held while a window is
  above a utilization and has not reset; a reading too old admits. A task
  whose ladder starts with a model refused until a time to come waits for
  it, whatever its priority (ADR 0052). Then the curve (ADR 0060,
  proposed, `mode: curve`): a line from zero at a window's start to a
  target at its reset, the rate from recent readings; below the priority,
  a task waits while a window is ahead of its line, with when it expects
  to run, and a window resetting soon with room admits all; `usage.room`
  says what a window will leave unused at the present rate. The dreamer and benchmark runs are
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
| Add detail to an entry, correcting nothing | `cmem note ... --adds-to ID TEXT` | laptop, worker |
| Report an entry as wrong | `cmem flag ID REASON` | laptop, worker |
| Record a finished task | `cmem record --outcome O < body` | laptop, worker |
| The list for a session's start | `cmem index` | laptop, worker |
| Entries close to a moment's text | `cmem flashback TEXT` | laptop, worker |
| Open a consolidation run | `cmem consolidate start PURPOSE` | dreamer |
| Read every current entry with sources and flags | `cmem consolidate entries` | all |
| Add a curated entry | `cmem add --run RUN ...` | dreamer |
| Replace or merge entries, whatever backs them | `cmem supersede ID --run RUN [--absorb ID] [--from ID] ...` | dreamer |
| Withdraw an entry | `cmem retire ID --run RUN REASON` | dreamer |
| Mark a correction as an addition, on the owner's word | `cmem relabel ID --run RUN [--note NOTE] REASON` | dreamer |
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
  with a pointer to the note. A note shorter than three quarters of a long
  entry may not replace it unless it says it stands for the whole (ADR 0025).
- A note that adds to an entry is stored beside it the same way, and the
  entry is marked "added to": not corrected, not disputed. The pointer says
  which it is (`relation: amends|adds`). A run can read an amendment as an
  addition; the relabel is a note of its own, undone with the run (ADR 0044).
- Memory is for what another agent on another task would use. One job's
  instructions belong in its definition, the present state of a system in
  that system, the reason for a design in the project's decision records;
  sessions are told so at their start (ADR 0044).
- A replacement by a working agent is for whoever the old entry was for.
  `--global` widens it and `--scope` narrows it; `--here` does neither (ADRs
  0021 and 0026).
- A search made in a session looks at what applies there. When another
  repository's entry matches better than anything found, the answer names
  that repository and how to search it, and shows none of its text.
- The consolidation role can add, supersede (which includes merging),
  retire and relabel, and nothing else. Every change names a run. A run is capped at 20
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
- On a shared, managed cluster (ADR 0033, built in the chart, not yet run
  there): published images, every pod under the restricted standard, the
  namespace closed to others, tokens made in the cluster, and workers
  reaching the service inside it. The laptop is to come in over a private
  network whose client runs inside the memory pod (not built).
- Agents run beside the memory as jobs or, when interactive, as a standing
  pod with a persistent home and a tmux session a person attaches to (ADR
  0034). Named sessions in it are opened again on their conversations when
  the pod starts. Every agent that does ordinary work has the memory hook.
- A job's conversation is kept, and is resumed as the agent that had it
  (ADR 0040): `scripts/deploy.sh CONTEXT agent resume ID` makes a
  pod from that agent's own definition, opens the harness on the
  conversation in tmux, with the job's model, tools and memory role, and
  attaches. The pod ends when the harness and its shell are left. A
  conversation is open in one place at a time, by a lock beside it. Where
  this leads: a session (a conversation, a working directory and a
  profile) is what lasts, and a pod is where it runs for a while; a
  dispatcher will start one where a message arrives and end it when idle.
- The dreamer is a party to messages where the dispatcher is on (ADR
  0070): a message to `dreamer` resumes or starts a
  conversation of its with its own restrictions; it writes only to the
  owner and in answer; its questions for the owner are a message from the
  conversation that wrote them, and his reply resumes it.
- A job's pod, and so a resumed conversation's, finds the harness already
  there (ADR 0041): it starts from a warm image, the agent image
  with the harness added as one layer outside the home, which a job of the
  installation's own makes in the cluster and keeps in the installation's
  private registry. The job checks the harness's channel twice a day and
  rebuilds only when its version or the agent image changed; pods name a
  tag it moves. Its start links that copy where the installer would have put
  it, and installs as before when the copy is missing or does not run. An
  interactive agent keeps its own copy.
- Agents leave the cluster only through the egress proxy, which holds every
  credential and puts it on the request for its own host (ADR 0031, 0035).
  An installation may have several accounts for the model, their
  credentials on the owner's machine; one is at the proxy, changed with
  `scripts/deploy.sh CONTEXT model-account NAME`, and usage readings count
  only since the latest change (ADR 0055, proposed).
  The model's credential is held by a meter beside the proxy (ADR 0050):
  the proxy hands it every request to the model's API with the
  caller's address, the meter refuses an address that is no agent's pod of
  the namespace, puts the credential on the rest, and adds up from the
  answers what each agent's pod used, by model and kind of token. Those
  records reach the dispatcher's log as `model.usage`, from the proxy's pod
  as the meter role, never through an agent; `dispatch status` shows them
  by agent beside what runs say of themselves, and where the two differ,
  with the kind of difference: a run counts every call its harness's
  process made and says how it counted (`basis`), so that one cut short,
  or recorded the old way, is told from one that disagrees.
  An agent may be held there to a limit in weighted tokens a day or a pod
  (`proxyLimit`, the owner's `dispatcher limit`), a call over it refused
  with a 402 the harness shows as it is (ADR
  0064, proposed).
- Beside the proxy and the memory, agents reach the namespaces the
  installation names for its own services, and have one namespace where
  they may run what a task needs, bounded by a quota and closed to the
  node and the internet (ADR 0037). A trial installation of the platform
  runs there, from a checkout, with its own tokens and no real credential
  (ADR 0054, proposed); `agent.sandbox.trial` lets its dispatcher start Jobs.
  Several sessions' trials stand side by side there, each one's objects
  named with its name as a prefix, and its dispatcher narrowed to its label.
- The installation's definition can be read and changed from within (ADR
  0056, proposed): an agent with `definition: true` has the deployed
  definition's bundle read-only, clones it (`agent-definition`), and sends
  its commits as a patch in a message; `scripts/deploy.sh CONTEXT
  definition apply MESSAGE-ID` on the owner's machine applies them as the
  same commits, refusing what a change to the definition may not touch,
  and deploys nothing.
- Nothing the chart puts on a cluster carries the project's name, and
  `scripts/chart-export.sh` writes the chart out under another (ADR 0032).
- One token per role, generated at the first deploy and kept in a Secret.
- Backup: a nightly job copies the installation's state to a store the
  platform gives: a restic repository in a bucket, with each night kept as
  a snapshot (ADR 0038). The library is copied as one file, after
  proving a library can be made of it; the dispatcher's database as a
  dump; the installation's definition; the conversations jobs have kept
  (`conversations`) and, for each interactive agent, its sessions
  (`sessions-NAME`): the harness's transcripts and what opens them again,
  not the checkouts and caches of its home. Those two are read from their
  volumes, mounted read-only in the job's pod, as a tar in a stable order so
  that restic stores what was added; a transcript is cut at its last whole
  line. An installation names what else.
  `cmem backup FILE` and `cmem restore FILE DIR` do the library by hand, on
  any machine, and `deploy/RECOVERY.md` has the steps for starting,
  checking and restoring.
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
- **Task stream (its harness built and piloted, its tasks not yet; ADR
  0069, proposed).** `bench/stream` is the world
  service and the scoring, `scripts/stream.sh CONTEXT run ARM` works an
  arm through a stream in a trial of its own; the pilot's six tasks ran on
  the cheapest model. The stream's forty tasks and the built-in arm are to
  come, and the run that answers is the owner's to start. A made world worked through over days. A world service with
  no model plays the systems an agent meets (a registry, a deploy
  interface, tickets), with rules written down nowhere, learned by acting.
  A task is a ticket, a fresh session of a trial's agent started by the
  trial's dispatcher; only the arm's memory passes between tasks. The
  world keeps the score: success and the wrong calls before it, with no
  model judging. Forty tasks over five days, in one order: lessons met by
  doing and needed again a day and three days on, lessons told once, rules
  that change, the tempting wrong answer, a wrong memory, and neutral
  tasks that memory should not touch. Arms, with the same model, harness
  and wording, each a trial of its own (ADR 0054, 0065): no memory,
  built-in memory, cmem filled by the sessions, and cmem with the
  dreamer's routines between days. Measured from what the agent does not
  write: the world's verdict, the meter's tokens for each task, the memory
  service's record of what a session was shown, read and wrote, and harm
  (neutral tasks failed, old rules followed, a wrong memory followed).
  Three runs an arm tell arms fifteen points apart; the ADR says what
  would count as memory not helping, written before any run. The run that
  can answer is on Sonnet (on the cheapest model the agent does not write
  lessons down), about $75 at list price; a pilot of the harness on the
  cheapest model is under a dollar.

Continual Learning Bench (`bench/clbench`) was the first plan for the
stream. Its pilot was inconclusive: the task was noisy, and the arms were
confounded by differently worded instructions (since fixed, and the
lesson is kept: one frame of wording for every arm).

M8 is checked by running the carman arm on a second harness with only the CLI.

## 8. Open questions

| # | Question |
|---|---|
| Q1 | Embeddings: only if the retrieval set shows word search is the limit. Evidence so far says the quality of what is written matters more than ranking |
| Q2 | Worker language: thin TS/Python on the SDK, or Go driving the CLI? |
| Q3 | Which tasks get a daydreamer pass, and on which model? |
| Q4 | Human approval gates (R9). ADR 0058 has the first gate, and proposes wording |
| Q5 | Answered: built-in Claude Code memory is off on the laptop since the hook was installed |
| Q6 | Answered by ADRs 0033 and 0038: no git remote; a nightly job copies the whole library to a store outside the cluster |
| Q7 | Should the laptop record episodes automatically, or only write notes? |
| Q8 | Answered by ADR 0023: a new note says `--here` or `--global`. Still open within it: a level for one organisation's repositories, between a single repository and everywhere |
| Q9 | Self-improvement: carman should notice how it is working and use good opportunities to improve itself, starting with memory consolidation. A first design is ADR 0058 (proposed): a list in the dispatcher's log, a surveyor that reads the log's evidence, and a gate on what starts by itself. Open: what it may change on its own beyond handing a brief to a builder |
| Q10 | Token budgets: agents that know the subscription's usage limits and when each window resets, a budget for each task, and in the end one total budget divided between tasks for the most value. Section 5 is its start |
| Q11 | Reaching the owner: prompts and summaries about what needs or is worth their attention, such as a question only they can answer. The channel is open (tickets and a chat bot were named) |
| Q12 | Which model does which memory processing: to be measured on a benchmark that tells models apart (ADR 0027) |

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
| DBOS Transact for the dispatcher | Built and compared. Recovery of a dead instance's work and of another build's is not in its open-source edition; a second state machine and 13 tables beside ours (ADR 0039) |
| River for the dispatcher | Built and compared. Sat beside our tables: its scheduling and crash rescue are too coarse for deadlines, and it has no global limit or waiting for an event (ADR 0039) |

## 11. Build order

1. Done: the memory library, the HTTP API, the `cmem` CLI, secret rejection,
   the session index, flashback, the Claude Code hook, the retrieval set, the
   transfer benchmark, replacement by working agents, consolidation runs with
   retire and revert (M4), the consolidation routine, and import from Claude
   Code's built-in memory.
2. Done: the service on the local cluster. Next: install the hook for the
   owner's own sessions, import the built-in memories, and start using it.
   The backup of the library is a nightly job's (ADR 0038).
3. A memory larger than the index: the scale test, and the cheap-model brief
   if it shows the tail is not being found (ADR 0017).
4. Deploy the service to the cluster and reach it over Tailscale (R4).
5. A second harness on the benchmark (M8).
6. The task-stream benchmark (M7), if the transfer benchmark stops being
   enough to decide on.
7. The dreamer, as the first agent carman runs by itself: the consolidation
   routine in a container, on a schedule, beside the memory (ADR 0027). Built: the agent image, the routine runner, the CronJob, and
   the commands that switch it on and run it once, and its model credential
   as a secret in the cluster (ADR 0028). It has run once on a real library.
   It reads the installation's definitions and may be given read-only MCP
   tools to check a claim; it is told its limits (ADR 0044). (2026-11-25)
   It may also be given command prefixes that only read, such as
   `gh pr view` (ADR 0046, proposed). Any agent may be held the same
   way, by naming its tools in its entry (ADR 0061, proposed).
   Not yet: distilling long documents, a comparison of models, a check on
   unattended runs.
8. Walking skeleton of the task path: one trigger, one worker, one verifier,
   `cmem record` at the end. The dreamer's pieces are its start.
9. Dispatcher pacing and the task ledger's wait-and-wake routing. Built: the
   dispatcher's contract, its acceptance suite, and an implementation on
   tables of its own with migrations, retention, a low idle cost and
   deadlines on calls out (ADR 0039); and the service on the cluster, with
   roles, Jobs from CronJob templates, a watcher that records every Job's
   end, its own Postgres and its dump in the nightly backup (ADR 0042).
   Then (ADR 0043): schedules by time of day from the values, expectations
   of the CronJobs it does not start, notices to the owner as tasks
   (withheld until switched on), hourly usage readings and each run's cost,
   and a first admission rule. Then (ADR 0045): messages between
   sessions, agents and the owner, each pod known by a token of its own.
   Then (ADR 0049): the owner's own sessions, as a place with a
   token of its own, through the same hooks. Then (ADR 0051, proposed): the
   owner's token off the cluster, which holds only its hash. Then
   (ADR 0053, proposed): a standing pod's watcher of its own sessions.
   Then (ADR 0058, proposed): the list of improvements to the platform,
   its evidence and its gate, tried in a trial; the surveyor is not yet
   an agent of any installation.
   Then (ADR 0063, proposed): rules from the log to tasks, watches and
   hooks for what happens outside, tried in a trial.
   Next: move the scheduled agents onto the dispatcher one by one, switch
   notices on, and act on room a window will leave unused (the curve:
   ADR 0060, proposed).

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
