# 0009. Requirements are human-owned

Status: Accepted (2026-11-17)

## Context

The owner wants to build this system with agents working from goals that
humans have approved. The requirements are the skeleton of "done"; if agents
could change them, they could change what done means.

## Decision

Requirements live in `REQUIREMENTS.md`, separate from the design. Only humans
edit it, and a human's commit is the approval. Each requirement has a "Done
when" check. Agents build against the file and never edit it.

## Consequences

- `DESIGN.md` and the ADRs may be written by agents; `REQUIREMENTS.md` may not.
- Tests name the requirement ID they check, so coverage of the file can be
  listed.
- When a requirement is wrong or missing, an agent proposes wording and a human
  applies it.
- Enforcement is in layers. `CLAUDE.md` states the rule. A deny rule in
  `.claude/settings.json` blocks the file-editing tools. Neither stops a shell
  command, so once the repository is on GitHub, a CODEOWNERS entry and branch
  protection should require the owner's review for changes to this file.
