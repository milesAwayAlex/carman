# The task-stream benchmark

Whether agents working through a stream of tasks over days do better with
memory than without (M7). The design, what would count as memory not
helping, and what the pilot showed are in
`docs/adr/0069-task-stream-benchmark.md`.

- `world.go`, `main.go`: the world service and the scoring (`stream world`,
  `stream admin`, `stream score`). The world has no model; its rules are
  code here, and nowhere in an agent's pod.
- `pilot.go`, `pilot.json`: the pilot's world and its six tasks, which try
  the harness. The stream's own tasks are not among them.
- `stream.go`, `stream.json`, `stream.seeds.json`: the stream's world, its
  forty tasks over five days, and the two wrong notes memory starts with.
  They are fixed: an arm with memory has run on them.
- `all.py`, `limit.py`: every arm so many times, two at a time, under a
  budget (`scripts/stream.sh CONTEXT all`); and whether a task's run stopped
  at the plan's limit, for the runner's wait.
- `collect.py`, `report.py`: what a run showed, from what the agent did not
  write, and runs set side by side by arm.
- `ops`: the agent's command for the world, put on its path in the trial.
- `scripts/stream.sh CONTEXT run ARM`: one arm through a stream, in a trial
  of its own; `report DIR...` sets runs side by side.

```sh
export TRIAL_MODEL=http://egress.NAMESPACE.svc:3131 TRIAL_AGENT_BASE=IMAGE-WITH-THE-HARNESS
scripts/stream.sh sandbox run none                    # the pilot's six tasks
scripts/stream.sh sandbox run tidied --world stream   # the stream's forty; arms: none, builtin, notes, tidied
scripts/stream.sh sandbox report ~/carman-bench/stream/*-stream-*
```

It calls a model: on the cheapest, the pilot is about two cents an arm.
Results (the world's state, each task's result, the memory service's log,
the library as it ended) are under `$CARMAN_BENCH_ROOT/stream/`, not in
this repository.
