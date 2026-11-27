# 0061: Any agent may be held to named tools and commands, as the dreamer is

Status: Proposed (2026-11-27).

## Context

The dreamer runs restricted (ADR 0027, 0044, 0046): the memory command,
the directories and the MCP tools the installation names, command prefixes
that only read, and nothing else. What it may not do is refused, not
asked about, and it is told its limits. Every other agent entry had two
choices: `tools: memory` (the dreamer's kind, with the memory command and,
where the pod can message, the dispatcher's whole client) or `tools: all`
(every tool, with only the pod's walls around it).

The improvement loop's surveyor (ADR 0058, `routines/improve.md`) reads
evidence others wrote, including the words of their messages. Text written
by others can carry instructions. Its routine needs one command,
`dispatcher improve`, and nothing on offer gave it only that: `memory`
would have given it `dispatcher send` as well, and `all` gives it
everything. The list of improvements has this as
`restricted-agent-with-named-commands`.

## Decision

An agent's `tools:` may be a map that names everything the agent may do:

```yaml
tools:
  memory: true                  # the memory command (off unless said)
  run: [dispatcher improve]     # commands with any subcommands and arguments
  commands: [gh pr view]        # prefixes that only read (ADR 0046's test)
  mcp: {tracker: [get_issue]}   # tools of the agent's own mcp servers
  read: [/etc/agent/routines]   # directories in its pod
```

`tools: all` and `tools: memory` mean what they meant. A map renders to
`CMEM_ROUTINE_TOOLS=named` and the variables `cmem routine` already reads
for the dreamer (`CMEM_ROUTINE_COMMANDS`, `_MCP_TOOLS`, `_READ`), with two
new ones: `CMEM_ROUTINE_MEMORY` (`on` or `off`) and `CMEM_ROUTINE_RUN`.
The harness gets the same flags as the dreamer's (`dontAsk`, `--tools`,
`--allowedTools`, `--strict-mcp-config`, a note of its limits in the
system prompt), and so does a run started for a message, through
`cmem routine --flags`. What is different:

- **Nothing by default.** A named agent has no memory command unless
  `memory: true`, and the dispatcher's client only as far as `run` names
  it. With no command named at all, it has no shell.
- **`run`: a command with its subcommands.** Each entry is allowed as
  `Bash(P)` and `Bash(P *)`. It is two to four lowercase words, as a
  read-only prefix is, and its program is not one that runs another
  command; no read verb is asked of its last word. One word is refused:
  it would allow a program whole.
- **The chart refuses at the deploy** what `cmem routine` would leave
  out: an unknown key, a `commands` entry that is not plainly a read, a
  `run` entry that is not a command, an MCP server the agent does not
  have, a directory not from the root, and named tools on an interactive
  agent (a person at the keyboard has `permissionMode`).
- **A restricted job no longer gets the image's allow rules.** The agent
  image puts allow rules for parts of `cmem` and `dispatcher` (`send`,
  `reply` and others) in every home's settings, for interactive sessions.
  The harness adds those to the allow rules it is given, so a restricted
  job had them too. Measured on 2026-11-27: with the image's rules, the
  surveyor's flags let `dispatcher send owner hi` and `cmem recall` run.
  Now a job that is not interactive and not `tools: all` gets only what
  its flags allow. The dreamer is one of those jobs: its own flags allow
  `cmem *` and, where it can message, `dispatcher *`, so it loses
  nothing.

The surveyor of the example values and of the trial is held to
`run: [dispatcher improve]`.

What was measured, on 2026-11-27, with the harness (2.1.326) and the
cheapest model, the flags `cmem routine --flags` prints for the surveyor,
a home with no settings of its own, and stand-ins for `dispatcher`,
`gh` and `cmem` that record what ran. These ran: `dispatcher improve list`,
`dispatcher improve` with nothing after it, `dispatcher improve evidence
--event 5`, `dispatcher improve brief x --to agent/b - <<'BRIEF' ...` (the
heredoc's text reached it), `dispatcher improve list | head -1`, two of
its commands joined by `&&`, and `echo hi | dispatcher improve brief ...`.
These were refused, and nothing in them ran: `dispatcher send owner hi`,
`gh api user`, `dispatcher improve list; dispatcher send owner hi`,
`cmem recall x`, `dispatcher improvement list`, `ls /`.

## What this protects against, and what it does not

It holds an agent to the commands and tools it was given, whatever the
text it reads says. An instruction planted in a message it reads cannot
make it send a message, write to memory, run another program, read files
outside the named directories or reach the network itself. A planted
instruction gets only what the agent was given.

It does not protect against:

- **An allowed command that itself does harm.** `run` allows every
  subcommand and argument. The surveyor may write any item onto the list,
  add any evidence, and ask for a brief with any text. A planted
  instruction can make it do any of those. What limits the harm is
  outside the harness: the dispatcher lets a brief through only by rule
  (an item the owner agreed, a named addressee, a budget, room in usage)
  and withholds it until `improvement.send`, and only the owner agrees.
  A command named under `run` should be one whose every form is
  acceptable from an agent that has been misled.
- **What an allowed MCP tool can write.** An MCP tool is allowed by name,
  and the credential behind it is the proxy's, which may write (ADR 0044).
  A tool called `get_issue` that writes would pass, as would a `list`
  verb that writes in `commands` (ADR 0046).
- **What an allowed command prints.** Text an allowed command prints
  enters the conversation like the text it was asked to read.
- **A command that reaches what others wrote by another way.** A `read`
  directory or an MCP tool that returns others' words widens what the
  agent reads, not what it can do.
- **A chain of an allowed program and a harmless one.** `echo hi | P`
  ran; the harness judges the parts of a pipe, and lets common harmless
  programs through. This was not found to admit anything harmful, but it
  is the harness's rule, not this project's.

## What an installation should choose

An agent that reads what others wrote (messages, reports, tickets, pull
requests, pages from outside) should be named, not `all`: only the
commands its routine uses, under `run` where it needs to write and
`commands` where it only reads, and memory only if it must note
something. Each command under `run` should be one whose every form is
acceptable from a misled agent, or one whose effect is held back by a rule
outside the agent, as the surveyor's briefs are. `tools: all` stays for an
agent whose input is its own task and its own repository, where the pod's
walls are the right limit.

## Consequences

- A named agent cannot use a skill, a plugin's command or a tool not on
  its list. A routine that needs one has its entry changed, not the rule.
- Interactive agents cannot be named. A person at the keyboard sets
  `permissionMode`.
- `read` names directories the pod already has. The chart mounts nothing
  for it.
- The surveyor's limits were not seen with a model inside a pod: the
  trial runs no model. The trial reads the surveyor's CronJob as applied
  and checks the flags derived from it (`scripts/check-improve.sh`), and
  the harness's behaviour under those flags was measured outside a pod,
  as above.
