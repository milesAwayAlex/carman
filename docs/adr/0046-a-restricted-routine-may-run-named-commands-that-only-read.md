# 0046: A restricted routine may run named commands that only read

Status: Proposed (2026-11-25).

## Context

The dreamer runs restricted (ADR 0044): the memory command, the
directories and the MCP tools the installation names, and nothing else.
Measured on 2026-11-25, two of six distilling passes said they had no way
to see a pull request's state on the code host, so a claim about one stayed
a question for the owner. The agent image has `gh`, and the egress proxy
holds its credential, which can write.

## Decision

A fourth thing a restricted routine may be given: command prefixes that
only read, set by the installation (`dreamer.commands` in the values, for
example `[gh pr view, gh pr list, gh issue view]`). `cmem routine` reads
them from `CMEM_ROUTINE_COMMANDS` (separated by commas), passes each to the
harness as an allow rule beside the others (`Bash(gh pr view *)`), and
names them in the limits the agent is told. `agent-resume --hold` gets the
same through `cmem routine --flags`.

**What counts as plainly a read.** The installation is trusted; the test
is there so that a typo that would allow a program whole does not pass
silently. A prefix passes when:

- it is two to four lowercase words (`[a-z0-9][a-z0-9._-]*`), one space
  apart: a program and one to three subcommands, with no flag, path,
  quote, glob or shell character;
- its program is not one that runs another command it is given (`sh`,
  `env`, `sudo`, `xargs`, `find`, an interpreter and the like);
- its last word is a verb that reads: `view`, `list`, `ls`, `show`, `get`,
  `status`, `diff`, `log`, `describe`, `search`, `checks`.

So `gh pr view` and `git log` pass; `gh`, `gh pr`, `gh pr edit`, `gh api`
(which can write) and `gh pr view --web` do not. The chart fails the deploy
on a prefix that does not pass (the first and last rules, as a regular
expression); `cmem routine` applies all three, leaves out what fails and
says so on its standard error, which is in the run's log.

## Consequences

- A verb is a convention, not a guarantee: a tool whose `list` or `get`
  writes would pass. The test catches slips, not a hostile installation.
- What keeps the dreamer to reading is still the harness's allow rules,
  not the credential the proxy puts on its requests (as for MCP tools,
  ADR 0044).
- The rules under which joined commands run or are refused were measured
  for `Bash(cmem *)` (ADR 0044's table) and are assumed to hold for these
  prefixes; it was not measured again for them here (the session that
  built this had no model credential for a child harness).
- Only the dreamer takes them from the chart. An agent with every tool
  needs none.
