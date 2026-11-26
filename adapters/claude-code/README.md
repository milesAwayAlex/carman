# Using cmem from Claude Code

This wires the memory into Claude Code so that it reaches the agent without
the agent asking. One command, `cmem hook claude-code`, handles five hook
events:

| Event | What it adds to the context |
|---|---|
| `SessionStart` | The index of current entries for this repository, and how to use `cmem` |
| `UserPromptSubmit` | Entries that closely match the prompt, if any |
| `PreToolUse` (shell commands) | Entries about a program the command is about to run, such as `kubectl` or `terraform`, if any |
| `PostToolUseFailure` | Entries that closely match the failed command and its error, if any |
| `Stop` | Entries that closely match the agent's reply. If there are any, the turn continues once so the agent can revise before you read it |

Most moments add nothing. An entry is never shown twice in one session. If the
service cannot be reached the session is unaffected: the hook says so once, at
the start, so the agent can tell you, and is silent after that.

## Install

1. Put `cmem` on your PATH. With Nix, this repository's flake exports it as
   `packages.<system>.cmem`, and as `pkgs.carman.cmem` through
   `overlays.default`. Without, from this repository:
   `nix develop -c go build -o ~/.local/bin/cmem ./cmd/cmem`
2. Have the service running and note its address and token. The real one is
   deployed with `scripts/deploy.sh devvm` and answers at
   `http://127.0.0.1:8377`; `scripts/deploy.sh devvm env` writes its address
   and your token to `.local/cmem.env` (see `deploy/README.md`). For a trial
   that touches nothing real, `nix develop -c scripts/dev-service.sh` serves
   on `http://127.0.0.1:8378` with the laptop token `dev-laptop`.
3. Merge `settings.example.json` into `~/.claude/settings.json` for every
   project, or into a project's `.claude/settings.json` for one. Set `CMEM_URL`
   there to the service's address.
4. Give Claude Code the token without committing it: export `CMEM_TOKEN` in
   your shell profile, or put it under `env` in `.claude/settings.local.json`.
5. Start a new session. The first thing in its context should be a line
   beginning "Long-term memory (cmem)".

`CMEM_NOTE_SCOPE=ask`, which the example settings turn on, makes every new
note say who it is for: `--here` for something true of this repository only,
or `--global` for something that would also hold in another. A note that says
neither is refused with a line explaining the choice. Without the setting a
note with no scope is shown everywhere, and a lesson about one repository gets
applied in the others.

Optional settings:

- **More of the index.** Claude Code keeps 10,000 characters per hook, which
  is about 70 lessons. To give sessions more, list the hook several times
  under `SessionStart`, as `cmem hook --part 1 claude-code`, `cmem hook --part
  2 claude-code` and so on, and set `CMEM_INDEX_PARTS` to how many. Three
  parts is about what built-in memory loads.
- **Less of it.** `CMEM_INDEX_BUDGET` is a smaller number of characters.
- **A brief.** `CMEM_BRIEF_CMD` is a command that reads a question on standard
  input and answers with entry IDs, normally by asking a small model. When
  set, each new prompt has it choose from the entries the session has not
  seen. Raise the `UserPromptSubmit` hook's timeout to cover it.
- **Off for one session.** `CMEM_HOOK=off`. In the agent image it also
  switches off the dispatcher's hooks, so a harness an agent starts for a
  trial is not a session that messages reach.
- **What to do when memory is down.** `CMEM_UNREACHABLE_HINT` is a sentence
  added to what a starting session is told when the service cannot be
  reached: the command that diagnoses it on this machine, for instance.

## Bringing your built-in memories along

Claude Code keeps its own memories in `~/.claude/projects/PROJECT/memory`, one
directory per project. To copy a directory into cmem:

    cmem import --dry-run claude-code ~/.claude/projects/PROJECT/memory
    cmem import --human YOUR_NAME --scope "$(cmem scope ~/path/to/checkout)" claude-code ~/.claude/projects/PROJECT/memory

A directory mixes memories that apply everywhere with ones about its project.
Name the first kind with `--global FILE`; they are stored with no scope and
the rest with `--scope`. For a project you no longer work on, add
`--globals-only` to take just those. The entries are recorded as told by you.
Running it again adds nothing: what is already there is skipped. Anything that
looks like a credential is refused.

To have a session make the global-or-not call for every directory and show you
its list before importing, start one with `CMEM_URL` and `CMEM_TOKEN` set and
ask it to follow `routines/import-claude-code.md`, telling it your name, which
repositories are active, and which memory directories belong to each.

## Tidying memory

`scripts/consolidate.sh` runs one consolidation pass (`routines/consolidate.md`)
in a Claude Code session that can only run `cmem`. It merges duplicates and
retires entries that newer ones replaced. It needs the consolidation role's
token in `CMEM_DREAMER_TOKEN`. `cmem consolidate log` shows what a pass
changed, and `cmem consolidate revert RUN REASON` undoes all of it.

## Scope

Entries carry scope tags such as `repo:github.com/acme/billing`. The hook
works out the session's scope from the repository it runs in, so a session
sees entries for its own repository plus entries with no scope. A repository
is named by its `origin` remote, so the scope is the same in every clone and
worktree whatever the directory is called; `cmem scope` prints it. A
repository with no remote is named by the directory of its main checkout. Set
`CMEM_SCOPE` to override all of this, for example
`CMEM_SCOPE=repo:infra,team:platform`.

## Built-in memory

Claude Code's own memory keeps working beside this one, and with both on the
agent may write a lesson to either. To have a single memory, turn the built-in
one off with `"autoMemoryEnabled": false` in the same settings file, after
importing what it holds. The benchmark's cmem arms ran with it off.

## Turning it off

Remove the `hooks` block. The `cmem` commands still work when called by hand.

# Messages to and from your own sessions

The same way, a session on your own machine can be written to by the
installation's agents and sessions, and write to them (ADR 0049): the
dispatcher's hook carries a message into it between tool calls, at the end
of a turn, or by waking it when idle, as in an agent's pod. The settings are
in `dispatcher.settings.example.json`, beside the memory's.

What it is to everyone else: a session of the place your machine is called
(`dispatcher.host.place` in the values, `host` unless changed), named for
the directory it runs in, so the session in a checkout called `billing` is
`host/billing`. Set `DISPATCHER_SESSION_NAME` to name it otherwise. It may
write to any agent and any session; an agent may write to it only to answer
it, or when listed under `dispatcher.host.writtenBy`. A message to `owner`
from it is a notice, as from any session: it is not you. A message for one
of your sessions that is not running waits, and the next session of that
name to start there is handed it.

## Install

1. Put `dispatcher` on your PATH, beside `cmem`. With Nix, the flake exports
   it as `packages.<system>.dispatcher` (and `pkgs.carman.dispatcher`).
   Without: `CGO_ENABLED=0 nix develop -c go build -o ~/.local/bin/dispatcher ./cmd/dispatcher`.
   Nothing else is needed: no pod, no token file, no kubectl.
2. Have the dispatcher reachable from your machine. On the local cluster the
   local values make its service a LoadBalancer on port 8379, forwarded to
   `http://127.0.0.1:8379`. `scripts/deploy.sh CONTEXT env` writes
   `.local/dispatcher.env` with `DISPATCHER_URL`, the host role's
   `DISPATCHER_TOKEN` and `DISPATCHER_PLACE`. Check it from a shell:
   `. .local/dispatcher.env && dispatcher whoami` says `role host`.
3. Try it in one project first: merge `dispatcher.settings.example.json`
   into that project's `.claude/settings.local.json` (not committed), with
   the token added under `env` as `DISPATCHER_TOKEN`, and `DISPATCHER_URL`
   and `DISPATCHER_PLACE` as `.local/dispatcher.env` says. Start a new
   session there: the first thing in its context says "You can be written
   to", and names it `host/PROJECT`.
4. For every session: merge the same into `~/.claude/settings.json`, and
   put the token in the environment Claude Code starts from (your shell
   profile sourcing `.local/dispatcher.env`, for one) rather than in the
   file.

Each hook gives up after three seconds, and does nothing when the
dispatcher does not answer. `CMEM_HOOK=off` or `DISPATCHER_HOOK=off` in a
session's environment switches it off for that session.

## Taking it out

Remove the `dispatcher hook claude-code` entries (and the `dispatcher`
permissions and `DISPATCHER_*` variables) from the settings file you added
them to. Sessions that were registered show as not running in `dispatcher
peers` until the message retention forgets them; what was sent to them
stays in the log, and the owner can see what became of each with
`scripts/deploy.sh CONTEXT dispatch message ID`.
