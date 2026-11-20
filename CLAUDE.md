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
  isolation, secrets made by External Secrets Operator and a nightly backup
  to a bucket (ADR 0033, not yet run on one). Nothing it puts on a cluster
  carries the project's name, and `scripts/chart-export.sh NAME DIR` writes
  it out under another (ADR 0032). `.github/workflows/images.yml` publishes
  the two images.
- The dreamer: the first agent. `routines/consolidate.md` and
  `routines/distill.md` run nightly as a CronJob in the agent image
  (`deploy/agent`), through `cmem routine`, which retries what the memory
  refused up a ladder of models (ADRs 0027 to 0029).
  `scripts/deploy.sh CONTEXT dreamer run|on|off|log`.
- `flake.nix` and `nix/packages/`: the Nix packages. carman is meant to grow
  beyond memory, so each component is its own file under `nix/packages` and
  its own attribute; `cmem` is the first.

Not built yet: the way in from a laptop to a service on a shared cluster,
the task-stream benchmark (M7), a second harness (M8), and for agents that
do work outside memory: triggers other than a schedule, and a way for
results to reach the owner. The build
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
- Every test that checks a requirement names its ID (for example `M2`) so
  coverage of `REQUIREMENTS.md` can be listed.
- Work in thin slices, each ending with a check that can be run. Slice work
  happens on a `slice/NN-name` branch and is merged when its checks pass.
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

- All checks: `nix develop -c scripts/test.sh`
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
