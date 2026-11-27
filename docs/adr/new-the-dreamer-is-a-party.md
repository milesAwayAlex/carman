# ADR new-the-dreamer-is-a-party: The dreamer is a party to messages, writing only to the owner and in answer

Status: Proposed (2026-11-27). Narrows ADR 0036 and 0040 for the dreamer:
where it is a party to messages, its questions for the owner go to him as a
message, not as a notice telling him to resume its conversation.

## Context

Agents are written to by name and write (ADR 0045). The dreamer, rendered
by a template of its own, was outside that: its pod had no identity for the
dispatcher, its CronJob no label `agent`, and its memory hook was off, which
switched the dispatcher's hooks off with it. It could not be asked what it
did, and when a report of its had questions for the owner, `run-routine`
sent a notice telling him to resume its conversation (ADR 0036, 0040).

The dreamer reads all of memory. Whatever it writes can carry anything
memory holds.

## Decision

- **Addressable by name.** Where the dispatcher is on (and
  `dreamer.messages` is not false), the dreamer's pod carries the label
  `agent: dreamer`, a token the cluster makes for the dispatcher, and
  `DISPATCHER_HOOK=on`, which keeps the dispatcher's hooks on where
  `CMEM_HOOK=off` keeps memory's off. A message to `dreamer` starts a Job
  from its CronJob's template; `run-routine` hands it to `agent-resume
  --message`, which resumes the conversation (or starts one) with the
  dreamer's own flags (`cmem routine --flags`): its memory role, what it may
  read, run and call, and the `dispatcher` command, nothing more (ADR 0044,
  0046, 0061).
- **Who may write to it**: as ADR 0047 has it for any agent: the owner, the
  owner's machine's sessions (ADR 0049), its own sessions and an answer. No
  `writtenBy` for it is offered.
- **What it may send, and to whom**: to the owner, and answers to what it
  was sent. Nobody's `writtenBy` may name it (the chart refuses to render
  one), so the dispatcher refuses any other message of its. What memory
  holds then reaches only the owner, or whoever the owner's side let write
  to it, through the dispatcher's log.
- **The notice rule** (ADR 0048) is said to every restricted agent that may
  message: a message to the owner reaches him as a notice of its first line;
  that line is a summary, with no personal data, identifiers or keys.
- **Its questions for the owner** are sent by `run-routine`, not the model:
  the first line "The ROUTINE routine has questions for you", then the
  report from its questions on, from the conversation that wrote them. His
  `dispatch reply` goes to that session, which is not running, so the
  dispatcher resumes it as the dreamer, which acts on the answer with the
  dreamer's role.
- **Where messaging is off** (no dispatcher, or `dreamer.messages: false`)
  nothing changes: no label, no identity, the notice of ADR 0036.

## Consequences

- A run for a message is not held by the CronJob's `concurrencyPolicy`:
  it can run beside the nightly run, both changing memory. Not guarded
  here; the owner's answer usually comes well after the night's run.
- The dreamer may write to itself (an agent may), which starts a run of
  itself; the limit on runs started by messages (ADR 0045) bounds it.
- A message's body (the questions and the report around them) is in the
  dispatcher's log, which is the owner's; only its first line travels.
