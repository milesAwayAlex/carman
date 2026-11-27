# carman

An event-driven agent system with one shared long-term memory. The memory is
being built first; see "Proof of concept" in REQUIREMENTS.md.

Built so far:

- `internal/memory`: the library. Memory is markdown notes in a git repository,
  written once and never changed. Also search, flashback, the session index,
  secret rejection, replacement of an entry by a working agent, and
  consolidation runs (merge, retire, a cap, revert as a unit).
- `internal/server`: the HTTP API, with a bearer token per role.
- `internal/cli` and `cmd/cmem`: the `cmem` binary, both service and client,
  including the Claude Code hook (`cmem hook claude-code`) and the import of
  Claude Code's built-in memories (`cmem import`).
- `routines/consolidate.md`: the prompt a harness runs, with the consolidation
  credential, to tidy memory. `scripts/consolidate.sh` runs it in Claude Code.
- `routines/import-claude-code.md`: the prompt for moving Claude Code's
  built-in memories into cmem, deciding per file what applies everywhere.
- `bench/retrieval`: the retrieval set, the first tier of the memory benchmark.
- `bench/transfer`: a small benchmark of whether a lesson learned in one
  session is used in another, in the same repository and in a different one.
  Its `consolidate` command tests the consolidation routine on a messy
  library. Results are in `bench/transfer/RESULTS.md`.
- `bench/clbench`: arms for Continual Learning Bench. Its first pilot was
  inconclusive; `bench/transfer` is the benchmark to trust for now.
- `adapters/claude-code`: how to install the hook.

- `charts/carman` and `deploy/`: the Helm chart for the memory service and
  the agents beside it, deployed with `scripts/deploy.sh CONTEXT`. It runs on
  the local cluster. For a shared, managed cluster it has placement, labels,
  isolation and secrets made by External Secrets Operator (ADR 0033, not
  yet run on one). A nightly job copies the installation's state to a store
  the platform gives, as a restic repository (ADR 0038): the library, the
  dispatcher's database, the installation's definition, the conversations
  jobs have kept and each interactive agent's sessions (not its checkouts;
  `scripts/check-backup-copy.sh` checks the copy without a cluster). Nothing
  it puts on a cluster carries the project's name, and
  `scripts/chart-export.sh NAME DIR` writes it out under another (ADR 0032). `.github/workflows/images.yml` publishes
  the two images.
- What memory is not for (ADR 0044): one job's instructions, the
  present state of a system, the reasons for a design. Sessions are told so;
  the dreamer reads the installation's definitions (`agents.yaml` and its
  routines) to retire a job's instruction its definition holds, and may be
  given read-only MCP tools or command prefixes (ADR 0046) to check a
  claim. `cmem note --adds-to` adds to
  an entry without marking it corrected; `cmem relabel` turns a correction
  into an addition, in a run. A restricted routine is told its limits.
- The dreamer: the first agent. `routines/consolidate.md` and
  `routines/distill.md` run nightly as a CronJob in the agent image
  (`deploy/agent`), through `cmem routine`, which retries what the memory
  refused up a ladder of models (ADRs 0027 to 0029).
  `scripts/deploy.sh CONTEXT dreamer run|on|off|log`.
- `flake.nix` and `nix/packages/`: the Nix packages. carman is meant to grow
  beyond memory, so each component is its own file under `nix/packages` and
  its own attribute; `cmem` is the first.

- Agents beyond the dreamer: entries under `agents` in the values, each a
  job on a schedule or on request (`scripts/deploy.sh CONTEXT agent run
  NAME`) or, with `interactive: true`, a standing pod with a persistent home
  and a tmux session to attach to (`agent attach NAME`, ADR 0034). They
  leave the cluster only through the egress proxy, which holds their
  credentials (ADR 0031) and renews the ones that do not last (ADR 0035),
  and can be given plugins, MCP servers, a cloud
  identity and commands of the installation's own. An entry's `tools:`
  is `all`, `memory`, or a map naming all it may do, as the dreamer is
  held (ADR 0061, proposed).

- Several accounts for the model (ADR 0055, proposed): the installation's
  definition lists them by name with the command that prints each one's
  credential on the owner's machine; `scripts/deploy.sh CONTEXT
  model-account [NAME]` says or changes which one the proxy holds, and
  usage readings count since the latest change.

- The model's credential at the proxy (ADR 0050): with
  `agent.modelCredential: proxy` agent pods hold a placeholder, and a meter
  beside the proxy puts the credential on each request to the model. It
  knows the caller by its pod's address, refuses a pod that is no agent's,
  and records what each agent's pod used, by model and kind of token, as
  `model.usage` in the dispatcher's log; `dispatch status` sets each run's
  own report against it. It reads the account's standing from the
  answers' headers as `usage.observed`, so the reading job asks only a
  model nobody called, and holds an agent to its `proxyLimit` in weighted
  tokens (`dispatcher limit`; ADR 0064). `pod` puts the credential back in the pods.

- The owner's attention (ADR 0036): an agent tells the owner with the
  installation's `notify-owner` command, and a job's conversation is kept
  so that the owner can resume it as the agent that had it, in a pod of
  its own (`scripts/deploy.sh CONTEXT agent resume ID`, ADR 0040). The dreamer's too: a report of its with questions for the
  owner sends one notice, or, where the dispatcher is on, a message he
  answers with `dispatch reply`; the dreamer is written to by name and
  writes only to the owner and in answer (ADR new-the-dreamer-is-a-party).

- The warm image (ADR 0041): agents' jobs start from the agent
  image with the harness already in it, made in the cluster by a job
  (`agent.warm`, `deploy/agent/warm-harness`, `scripts/deploy.sh CONTEXT
  warm run|log`) and kept in the installation's registry. On the local
  cluster it waits on the node's registry being reachable from a pod.

- Services on the cluster (ADR 0037): agents reach the namespaces the
  installation names (`agent.namespaces`), and have a namespace of their
  own to run what a task needs (`agent.sandbox`, kubectl context `sandbox`).
  A trial installation runs there (ADR 0054, proposed): `scripts/trial.sh
  CONTEXT up|check|deploy|list|down`, with `agent.sandbox.trial` for its
  dispatcher to start Jobs; each session's beside the others', its
  objects' names prefixed with its name.

- The dispatcher (ADR 0039, 0042): `cmd/dispatcher`, a service
  on the cluster with its own Postgres. It starts a task as a Job made from
  the job template of the CronJob its class names, and records in its log
  the end of every Job in the namespace, whoever started it. Its dump is in
  the nightly backup. `scripts/deploy.sh CONTEXT dispatch
  status|log|submit|dump|restore`.
- Schedules, notices and usage (ADR 0043): the dispatcher starts
  work on cron schedules from the values (`dispatcher.schedules`, or
  `dispatched: true` on an agent, whose CronJob is then a suspended
  template), records `run.missed` for a CronJob's tick with no Job, tells
  the owner of failures and of what did not happen through a `notify` task
  (withheld as `notice.withheld` until `dispatcher.notices.enabled`), and
  keeps the account's usage: hourly readings (`agent-usage`,
  `usage.observed`) of every model the definitions name, the cheapest
  every hour and the others every six hours, a refusal for a limit
  recorded as a reading (ADR 0052, proposed), and each routine run's cost
  (`run.usage`), shown first by `dispatch status`, each model's standing
  most constrained first. The dreamer, the backup and the warm job are
  moved onto it each by a value (`dispatched`), and places of its limit
  can be kept for scheduled work (`dispatcher.reserve`, ADR 0059,
  proposed). An admission rule that holds low-priority work
  while a usage window is high, and a task whose ladder starts with a
  refused model, is built and off; with `mode: curve` it holds it while a
  window is ahead of a line to its reset (ADR 0060, proposed), and
  `usage.room` says what a window will leave unused. A schedule, an agent or a
  submitted task may start `after` another (ADR 0062, proposed).

- Messages (ADR 0045): sessions, agents by name and the owner
  write to each other with `dispatcher peers|send|reply|inbox|message`
  (the owner: `scripts/deploy.sh CONTEXT dispatch send TO 'TEXT'`). A
  message is an event in the dispatcher's log; an agent's pod proves which
  agent it is with a token the cluster makes for the dispatcher; the
  harness's hooks carry a message into a running session (between tool
  calls, at the end of a turn, or waking an idle one), and a message for a
  session that is not running starts it again as its agent. Who may write
  to an agent (ADR 0047): the owner, its own sessions, an
  answer, and the agents its `writtenBy` lists.
- Sessions on the owner's own machine (ADR 0049): a place
  (`dispatcher.host.place`) with the dispatcher's `host` token, its
  sessions `PLACE/NAME` named for their directory. They write as the owner
  may but are not the owner; a message for one that is not running waits
  for the next session of its name there. The same hooks, installed from
  `adapters/claude-code` with `.local/dispatcher.env`
  (`scripts/deploy.sh CONTEXT env`); the dispatcher a LoadBalancer on the
  local cluster's port 8379.
- A standing agent's sessions (ADR 0053, proposed): `dispatcher attend`
  in its pod says when a session's harness process has gone (recorded by
  its `SessionStart` hook), and opens again with `agent-open --reopen` a
  remembered session a message waits for, with a backoff, telling the
  sender why when it does not.
- `carman go` (ADR 0057, proposed): `cmd/carman`, the owner's way to a
  session from any directory. It lists the dispatcher's sessions, those
  waiting on him first, and goes to one: `kubectl exec` into a standing
  agent's pod, at a view of its own (`agent-view`: a tmux session in the
  pod's group, with no status line and no prefix), attached again after a
  dropped connection with `--keep`.
- The definition from within (ADR 0056, proposed): an agent with
  `definition: true` reads the installation's definition in its pod
  (`agent-definition clone|update|propose`), and its proposal, a message
  holding a patch, is applied on the owner's machine by `scripts/deploy.sh
  CONTEXT definition apply MESSAGE-ID`, which deploys nothing.
- The owner's token off the cluster (ADR 0051, proposed): the dispatcher
  knows only its SHA-256; the token is in `.local/dispatcher-owner.token`,
  and `scripts/deploy.sh CONTEXT dispatch ...` runs the client on the
  owner's machine with it (`dispatch owner-token new|back`).

- A trial's agents reach the model (ADR 0065,
  proposed): with `egress.model.trials.enabled` the meter listens on a port
  of its own for trials, which serves the model and nothing else, knows a
  caller by its pod in the sandbox (labels `trial` and `agent`), records
  its use as `trial/TRIAL/AGENT`, and passes only the models and the count
  of requests a day the installation allows. `scripts/trial.sh` with
  `TRIAL_MODEL` and `TRIAL_AGENT_BASE` gives a trial's agents that address,
  a placeholder, and an image with the harness and the checkout's code.

- Triggers (ADR 0063, proposed): `dispatcher.rules` start a task of a
  class when an event of a kind is in the log (`internal/dispatch/trigger`):
  once per event, at most so often (`rule.held` otherwise), never for a
  rule's own run, the run told what triggered it in `TASK_TRIGGER_*`.
  `dispatcher.watches` are jobs with no model that ask a source on a
  schedule (`dispatcher source`) and tell the dispatcher a digest, which
  records `watch.changed`; `dispatcher.hooks` are doors at
  `/v1/hooks/NAME` for calls signed with the hook's secret. Only tokens,
  not text, are kept from outside and handed to a run.
  `scripts/check-triggers.sh CONTEXT` tries it all in a trial.

- The improvement loop (ADR 0058, proposed): the list of what should be
  improved in the platform is events in the dispatcher's log
  (`dispatcher improve`, the owner: `scripts/deploy.sh CONTEXT dispatch
  improve`); only the owner agrees or declines. A surveyor, an agent with
  the routine `routines/improve.md`, reads the evidence the log holds
  (`improve evidence`: a message's words only from senders the
  installation lists), adds to the list, and asks for a brief, which the
  dispatcher lets through by rule (agreed, a budget for the day, room in
  usage, a named addressee) and records without sending until
  `dispatcher.improvement.send`. `docs/improvements/seed.json` is what was
  known at the start; `scripts/check-improve.sh CONTEXT` tries it all in a
  trial. A deploy records what it deployed (`installation.deployed`: the
  commit and the subjects of the commits since), which is how the surveyor
  knows what is merged.

Not built yet: the way in from a laptop to a service on a shared cluster,
more than one machine of the owner's (one place per installation), the
watcher of ADR 0053 on the owner's machine,
the task-stream benchmark (M7), a second harness (M8), and for agents that
do work outside memory: rules that chain or keep events for later (ADR
0063), and a way for the
owner to answer an agent's message where it arrives (today: `dispatch reply`
from the laptop). The scheduled jobs
are not yet moved onto the dispatcher on a real installation, and
nothing yet acts on the room a window will leave unused. The build
order is in DESIGN.md, section 11.

## Read first

- `REQUIREMENTS.md`: what done means. Human-owned.
- `DESIGN.md`: the system as currently intended.
- `docs/adr/`: why each decision was made.

## Rules

- Never edit `REQUIREMENTS.md`. If a requirement looks wrong, missing or
  impossible to check, stop, say so, and propose wording for a human to apply.
- Accepted ADRs are not edited. To change a decision, add a new ADR that
  supersedes the old one, and change only the old one's status line.
- When work needs a decision that no document covers, write an ADR with status
  Proposed and ask. Do not choose silently.
- When a change alters the design, update `DESIGN.md` in the same change.
- An instruction about one agent's behaviour (what it does, how it reports,
  its model, its schedule) changes that agent's definition, and is not noted
  in memory (ADR 0044).
- Every test that checks a requirement names its ID (for example `M2`) so
  coverage of `REQUIREMENTS.md` can be listed.
- Work in thin slices, each ending with a check that can be run. Slice work
  happens on a `slice/NAME` branch and is merged when its checks pass. Choose
  no number: several sessions work at once. A new ADR is
  `docs/adr/new-SLUG.md`, called ADR new-SLUG, and whoever merges numbers it
  (`scripts/adr-number.sh`, which also moves a clashing number).
- This is a tool anyone can use. Keep what belongs to one person's setup out
  of code, tests and docs: organisation names, private repository or service
  names, incidents, home-directory paths. Settings for one machine go in the
  git-ignored `.local/`, and instructions for one machine in the git-ignored
  `CLAUDE.local.md`.
- Never act on a cluster through kubectl's default context. Name the context
  on every command (`kubectl --context NAME ...`); the deploy script takes it
  as its first argument for the same reason.

## Build and test

Go and git come from the Nix flake; nothing needs installing.

- All checks: `nix develop -c scripts/test.sh`. It names at the end what it
  skipped (no Postgres, helm, restic, tmux or python3); before a merge, run
  it with `--strict` first, which fails on a skip.
- One requirement's checks: `nix develop -c scripts/test.sh -run M2 -v`
- The retrieval benchmark: `nix develop -c scripts/bench-retrieval.sh`
- The transfer benchmark (calls a model; start with one case and read the
  transcripts): `scripts/bench-transfer.sh run --arms none,oracle --cases kubectl-context`
- The consolidation test (calls a model): `scripts/bench-transfer.sh consolidate`
- Format: `nix develop -c gofmt -w .`
- The Nix package: `nix build .#cmem`. When `go.sum` changes, its
  `vendorHash` in `nix/packages/cmem.nix` has to change with it.
- Deploy the real service to the local cluster: `nix develop -c scripts/deploy.sh devvm`
  (`deploy/README.md`). It answers at `http://127.0.0.1:8377`. Its library is
  the owner's memory and is write-once: never write test notes to it. Use the
  development service below for trying things.
- Run a development service: `nix develop -c scripts/dev-service.sh`, then in
  another shell `export CMEM_URL=http://127.0.0.1:8378 CMEM_TOKEN=dev-laptop`
  and use `bin/cmem` (`bin/cmem help` lists the commands). The library is in
  `.local/library`; open it in any markdown editor.
