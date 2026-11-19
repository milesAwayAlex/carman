# 0027: The dreamer is the first agent, and it runs on the cluster beside the memory

Status: Proposed

## Context

The build order (DESIGN.md, section 11) put the task path first (a trigger,
a worker, a verifier) and the dreamer last, "once there are enough episodes
and a benchmark to check it against".

A day of real use changed the order of need. The memory was filled by an
import and by working sessions, and what it lacks is processing: entries that
disagree, rules that a later rule made obsolete, corrections lying beside the
entries they correct, and working documents of 50,000 to 200,000 characters
that hold a handful of lessons among details nobody will want again. Every
one of these was found by a person or by a test agent reading the library.
The routine that tidies memory exists, is tested, and can be undone as a
unit (ADR 0018), but it runs only when someone runs it from a laptop.

The owner's direction: consolidation and the agent part are one piece of
work, because the dreamer is an agent with one job; and carman's agents
should be native to Kubernetes and run beside the memory, starting with it.

## Decision

- **The dreamer is built first**, ahead of the general task path. It is the
  walking skeleton of that path with a real job in place of a toy.
- **An agent is a routine run in a container.** The agent image holds a
  harness (Claude Code today), the `cmem` client and the routines, which are
  prompt files. `run-routine NAME` hands the routine to the harness with the
  memory command as the only thing it may run. What the agent is allowed to
  do is set by the role of the memory token it is given, not by the prompt.
- **The dreamer is a CronJob in carman's namespace**, next to the memory
  service: once a night, never two at once, with a deadline. It is given the
  consolidation role's token and a model credential from the cluster's
  secrets (ADR 0028). Its report is what it prints; the run it made is in
  memory's own log and one command undoes it.
- **The schedule starts switched off** and is a setting of the cluster: a
  deploy leaves it as it was. `scripts/deploy.sh CONTEXT dreamer on|off|run`.

## Consequences

- Section 11 of DESIGN.md is reordered: dreamer, then the task path.
- The second routine is expected to be a real recurring task (the owner
  named a weekly price review). It needs tools beyond memory, which means
  credentials for other services (ADR 0028) and a wider allow-list than
  "the memory command only".
- Nothing yet reads the dreamer's report but a person running `dreamer log`.
  Delivering it somewhere is to be decided.
- No verifier checks a run. The safeguards are the routine's own rules, the
  cap on entries per run, the log, and revert. A check on a schedule that
  nobody watches deserves more than that; see "Open".

## Open

- **Long documents.** The routine lists them and leaves them. The owner's
  rule: distil a document into the lessons worth keeping, keep the original
  in history marked as replaced, and leave a short entry saying what it was
  and how to read it. This needs a way for a distilled lesson to keep the
  document's backing, a rule for telling finished work from work in
  progress, and a benchmark case, and is the next slice.
- **An archive.** Separating active memory from past projects is deferred
  until there is a need.
- **A check on unattended runs.** For example: the run is reverted
  automatically if the library fails its own check afterwards, or if it
  retired more than it replaced.

## Alternatives considered

- **A scheduled job on the laptop.** Works today and needs no cluster
  credential, but runs only while the laptop is awake, and is not where
  carman's other agents will run.
- **A long-running dreamer service.** Nothing about consolidation needs a
  process between runs.
