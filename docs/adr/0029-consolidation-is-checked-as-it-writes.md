# 0029: Consolidation is checked as it writes; what fails is retried with the reason, then escalated

Status: Accepted (2026-11-19). Built: the check at the write, the record of
what was refused, retry with the reason, the ladder and the cap. Not built:
starting an entry at the rung it needed last time, and a second reader.

## Context

The dreamer runs with nobody watching (ADR 0027). Its routine says a merge
must keep every specific of the entries it merges, and the benchmark showed
what happens to a rule that is only said: every agent corrected about one
line of a long entry replaced the entry with a summary, and one summary
dropped the command the next session asked for. The same benchmark showed
what works. A question put by the tool at the moment of the write was
answered correctly every time it was asked.

The first design checked a run after it finished and reverted it on
failure. The owner's objection: a reverted run started again has the same
library and the same prompt, and makes the same mistake. And "stop and
leave it for a person" is not an answer for a system meant to run itself.

## Decision

- **The service checks a consolidation rewrite when it is written.** The
  numbers, the code and the identifiers found in the entries being replaced
  have to be in the new text. A write that lacks one is refused, and the
  refusal lists what is missing. The agent puts it right in the same
  session, with the texts still in front of it.
- **Leaving a specific out on purpose is said, and kept.** `--drops
  'SPECIFIC: why'`, once for each. It is for a value that a correction
  replaced. What a run dropped and why is in the run's log.
- **A failure that is found later carries its reason forward.** The failed
  change is undone, the reason is recorded in memory, and the next attempt
  begins from it. Only the failed change is done again.
- **Retry the same night, then escalate.** One more attempt by the same
  model with the reason in hand, then the next model up, as far as the
  strongest available. An entry that needed a stronger model starts there
  next time.
- **The ladder is bounded.** Each rung is one attempt, and a night has a
  spending cap. Which models are on it is a setting, to be fixed by
  measurement and not by guess.
- **A disagreement about the world is not escalated.** When two entries say
  different things about how something is, no model can tell which is true
  from the entries. The fix is evidence: an agent that can go and look,
  once there is one, and until then one specific question to a person.

## Consequences

- The check knows only what it can recognise without understanding: a
  rewrite that keeps every number and changes the meaning passes it. That
  is for a second reader, a model, to catch, and is not built.
- It errs towards refusing. A word with a capital in the middle is taken
  for an identifier whether or not it is one. The cost is a turn; the agent
  either keeps the word or says it drops it.
- It applies to the consolidation role's rewrites. A working agent's
  `--replaces` is a correction, which changes specifics by design, and has
  its own question about length (ADR 0025).
- Distilling a long document leaves most of its detail out on purpose, so
  it needs the check turned round: every specific in a lesson must be in
  the document it came from.

## Alternatives considered

- **Check after the run and revert it.** Catches the same thing a night
  late, and leaves the retry with nothing new to go on.
- **Leave whatever fails to a person.** Makes the owner the queue for a
  system whose point is not to need one.
- **A second model reviewing every change.** Worth having for meaning, but
  it costs a model call for a test a program does exactly.
