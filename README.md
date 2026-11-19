# carman

An event-driven agent system with one long-term memory that every task and
every machine shares. The memory is being built first, and is the only part
that exists so far.

## The memory: cmem

`cmem` is one binary: a small service, and the command-line client an agent
(or a person) uses from a shell.

- **Memory is markdown notes in a git repository.** Every note is written once
  and never changed; an entry changes by a new entry replacing it. You can
  read the library in any editor, and git is the audit trail.
- **The service calls no model.** It stores, searches and enforces rules.
  Anything that needs judgement, such as tidying memory, is a prompt that a
  harness runs with its own model (`routines/`).
- **Every entry says where it came from**: a person who said it, or a task
  that showed it, and whether that was verified.
- **It reaches the agent without being asked.** A Claude Code hook adds an
  index at the start of a session and brings up an entry at the moment it
  applies: a new prompt, a command about to run, a failure, or the agent's own
  reply.
- **Any harness with a shell can use it.** Nothing in it is specific to one
  agent framework.

## Does it work?

`bench/transfer` teaches an agent a lesson by correcting it in one repository,
then checks whether a fresh session uses the lesson, there and in another
repository. On three lessons with three runs each, Claude Code's built-in
memory, which is kept per repository, carried the lesson to the other
repository in 0 of 9 runs (7 of 9 when pointed at one shared directory), and
cmem with its hook in 9 of 9. The method, every result, and what has not been
tested are in [bench/transfer/RESULTS.md](bench/transfer/RESULTS.md). The
samples are small; read the caveats there.

## Try it

Go and git come from the Nix flake; nothing else needs installing.

    nix develop -c scripts/test.sh            # all checks
    nix develop -c scripts/dev-service.sh     # a service on 127.0.0.1:8378

    # in another shell
    export CMEM_URL=http://127.0.0.1:8378 CMEM_TOKEN=dev-laptop
    nix build .#cmem && ./result/bin/cmem help

- Using it from Claude Code: [adapters/claude-code](adapters/claude-code/README.md)
- Running the service on Kubernetes: [deploy](deploy/README.md)

## Where things are

| Path | What |
|---|---|
| `REQUIREMENTS.md` | What done means. Written and changed only by a person |
| `DESIGN.md` | The system as currently intended |
| `docs/adr/` | Why each decision was made, one per file |
| `internal/`, `cmd/cmem` | The memory library, the HTTP service, the CLI and the hook |
| `routines/` | Prompts a harness runs: consolidating memory, importing Claude Code's memories |
| `adapters/claude-code` | The hook's settings and install notes |
| `deploy/` | Kubernetes manifests |
| `bench/` | The benchmarks and their results |
| `nix/packages/` | Nix packages, one per component |

## How it is built

The requirements are owned by a person and the code is written by agents
against them. Decisions are recorded before or as they are made, and work
lands in small slices that each end with a check. `CLAUDE.md` holds the rules
the agents follow.

## Licence

MIT. See [LICENSE](LICENSE).
