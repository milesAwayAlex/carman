# Task-stream benchmark: four arms on Continual Learning Bench

The second tier of the memory benchmark (ADR 0013). It runs the tasks of
[Continual Learning Bench](https://github.com/pgasawa/continual-learning-bench)
with Claude Code as the agent, a fresh session for every task instance, and
only the arm's memory carried from one instance to the next.

## The arms

| Arm | Memory |
|---|---|
| `none` | None. Built-in memory is switched off |
| `builtin` | Claude Code's built-in memory directory |
| `cmem` | `cmem` on the PATH, used by hand (recall, note, flag). No hooks |
| `cmem_hook` | `cmem` plus the Claude Code hook: the session index and flashback |

All four use the same image, model and prompts. Each gets one paragraph about
memory built from the same frame, in which only the sentence describing the
mechanism differs; the no-memory arm gets a paragraph of the same kind saying
there is none. (The first pilot worded the arms differently, and the wording
alone changed the scores before any memory existed.)

Between instances everything except the arm's memory is removed: the
workspace, transcripts, temporary files.

Each cmem arm gets its own `cmem serve` on the host with a new, empty library,
and a new one again on every reset, so runs never share memory.

## Running it

```
nix develop -c scripts/bench-stream.sh dry-run exploitable_poker all   # no model calls
nix develop -c scripts/bench-stream.sh smoke   exploitable_poker all   # one short session per arm
nix develop -c scripts/bench-stream.sh run     database_exploration all --schedule default --runs 3
```

`smoke` and `run` call a model on the owner's Claude subscription. The default
model is `claude-haiku-5-5`; pass `--system.model NAME` to change it. Runs use
one worker at a time unless told otherwise.

The token is taken, in order, from `CLAUDE_CODE_OAUTH_TOKEN`, from the file
`.local/bench-token` (mode 600), or from 1Password through the reference that
`.local/bench.env` puts in `CARMAN_BENCH_TOKEN_REF`. It is never printed or
written to disk, and it reaches the containers by name only. The run refuses to
start if `ANTHROPIC_API_KEY` is set.

## Where things land

| What | Where |
|---|---|
| Scores and traces | `.local/clbench/results/<task>/` |
| Token usage, one line per `claude -p` call | `$CARMAN_BENCH_ROOT/usage/<arm>-<run>.jsonl` |
| What the agent wrote to cmem | `$CARMAN_BENCH_ROOT/libraries/<arm>-<time>-<id>/`, a git repository of markdown notes |
| Agent workspaces | `$CARMAN_BENCH_ROOT/workspaces/` |

`CARMAN_BENCH_ROOT` is `~/carman-bench` unless set, and has to be a directory
the container runtime can mount from. Where `docker` runs inside a virtual
machine that sees only one shared directory, point it there with a line in
`.local/bench.env`, which the scripts read:

    CARMAN_BENCH_ROOT=/path/the/vm/can/see/carman-bench

## What is here

- `systems/_carman/base.py`: the shared system class, a subclass of the
  benchmark's Claude Code adapter.
- `systems/_carman/arms.py`: the four arms and the paragraph each gives the agent.
- `systems/_carman/cmem_service.py`: starts and stops `cmem serve` on the host.
- `docker/`: the two images, Claude Code alone and Claude Code with `cmem`.
- `schedules/`: our own schedules for the benchmark's tasks (none yet).
- `report.py`: one line per finished run, and every failed run with its reason.
- `install.sh`: links the above into the benchmark clone. Nothing in the clone
  is edited; files are only added.
- `build-images.sh`: builds the images when they are missing.

## Rules for a run that can be trusted

- One benchmark at a time. The runner refuses to start beside another.
- At least three runs per arm. On the database task, two runs under
  effectively the same conditions have differed by 0.06.
- Read `report.py` before reading any score: a run that failed is listed
  there and must not be read as a low score.

## Known limits

- The arms are compared with each other, so the benchmark's own stateless
  baseline is switched off for them. `none` is the baseline.
- The benchmark prepends the feedback on the previous instance to the next
  prompt in every arm, including `none`. So `none` is "no memory beyond the
  last feedback", not fully stateless.
- Two of the benchmark's tasks start containers in a way `nerdctl` rejects and
  have not been patched: `codebase_adaptation` and `sales_prediction`.
- The virtual machine has 8 GiB of memory; keep to one to three workers.
