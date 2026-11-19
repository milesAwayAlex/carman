# 0020. In Claude Code the index is delivered within the hook's limit, in parts; a small model can read the rest

Status: Accepted (2026-11-18). Built. The scale test has since run once: every
arm found the lesson, and the brief was the cheapest (bench/transfer/RESULTS.md).
The adapter's default stays one part; the brief is off unless configured.

## Context

ADR 0017 raised the index to 24,000 characters to match what Claude Code's
built-in memory loads. The first scale test (a memory of 265 lessons) showed
that this does not reach the agent. Claude Code keeps at most 10,000
characters of what one hook adds to the context; a longer output is saved to
a file and replaced by a preview of its first 2,000 characters, and the agent
is not told to read the file. So past about 70 lessons a session saw roughly
the first dozen entries, and the arm whose index nominally held every lesson
failed both probes.

The same run showed how built-in memory copes with its own limit. Told that
its index was too long, the agent rewrote it during the teach session into a
short list plus 16 topic files. That cost one session $0.20 and nine turns.

## Decision

- **No hook output exceeds the limit.** The Claude Code hook lays the index
  out in texts of at most 9,500 characters and cuts what does not fit. The
  first line says how many entries there are and how many are shown.
- **The index can be delivered in parts.** Claude Code measures each hook's
  output separately, so the adapter may list the hook several times at session
  start, as `cmem hook --part 1 claude-code`, `--part 2` and so on, with
  `CMEM_INDEX_PARTS` saying how many. Three parts is about what built-in
  memory loads.
- **Only what was delivered counts as shown.** The hook asks for the index
  without naming the session, then records the entries it actually printed.
  Entries that were cut stay available to flashback and to the brief.
- **The brief** (ADR 0017, point 5). When `CMEM_BRIEF_CMD` is set, on each new
  prompt the hook asks the service for the entries the session has not seen,
  passes them and the prompt to that command on standard input, and prints the
  entries whose IDs the command answers with, as written. The command is the
  harness's own way of asking a small model. `cmem` contains no model client
  (M1, ADR 0006). If the command fails, the hook falls back to matching words.
- The service's own default budget stays 24,000 characters. It is a ceiling
  for harnesses with no such limit; the Claude Code hook asks for what fits.

## Consequences

- With one part, a Claude Code session starts with about 70 lessons, a third
  of what built-in memory loads. Parity needs three parts, which is three hook
  processes at session start.
- Results before this change stand: every memory in them fitted under the
  limit. The 24,000-character default was live for a few hours and would have
  hidden most of a memory of more than about 70 lessons.
- A brief is a model session started from inside a hook. In the benchmark's
  container the first attempt was refused as not logged in, so how the
  harness's credential reaches that session has to be settled per harness. On
  the owner's laptop the credential is injected by a shell function, which a
  hook's command does not pass through.
- Each prompt that leaves unseen entries costs one small-model call.
- Built-in memory's answer to overflow, a topic directory written by the agent
  itself, is an option for our consolidation routine too: it could maintain
  topic entries that stand for groups of lessons.
