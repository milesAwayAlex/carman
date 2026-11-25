# 0044: What memory is not for; a note that adds to an entry; what the dreamer may read and check

Status: Accepted (2026-11-25; proposed 2026-11-24). The owner: "i scanned them, looks okay, let's test the system in practice". Built; not yet deployed.

## Context

The dreamer's nightly report asked the owner four questions. The owner
resumed its conversation (ADR 0040) and answered, and three things came of
it.

- One question was about a scheduled job: two entries disagreed about how
  the job delivers its result. The owner: "why does the dreamer ask me about
  [it]? why is that in memory? it should live in the job definition." The
  job's definition already said the right thing. Memory held older copies of
  instructions for that one job, recorded because sessions are told to
  record whatever the user tells them.
- On an entry that said a ticket was still open, beside one that said it was
  closed: "you should have access to [the tracker] to check". The dreamer
  had no way to look.
- On an entry marked "corrected in part" by a note that only added detail:
  nothing was corrected, and the label should go. Memory had no other word
  for a note beside an entry (ADR 0025).

And the resumed session stopped. The dreamer joined its memory commands and
another program in one shell call; the harness refused the call, and the
dreamer concluded that its shell was taken away and did nothing more.

## Decision

**What memory is not for.** Memory holds what a different agent, doing a
different task, would use: how the person likes to work, lessons and
pitfalls, knowledge of a domain any agent working there would want.
Elsewhere:

- what one job or agent must do, how it reports, which model, when: its
  definition;
- the present state of something: the system that knows (the cluster, the
  dispatcher's log, the tracker), never memory;
- why something is built as it is: that project's decision records.

Every session is told so at its start, in one sentence, and `cmem help` says
the same where notes are explained. The distilling routine does not write a
lesson that is one job's instruction.

**The consolidation routine applies it.** An entry that is an instruction for
one job, where the job's definition already says it, is retired with a
reason that names the file and quotes it. Where the definition does not say
it, or says otherwise, the entry stays and the owner is asked in one line:
an instruction to put in the definition, or one out of date? It is never
retired then. An entry that is a snapshot of a system's state is listed for
the owner unless a later entry replaces it.

**The dreamer may read the installation's definitions.** The chart puts
`agents.yaml` (each agent's routines, schedule, models, tools, MCP servers
and whether it has a cloud identity; not its environment, not the node's
directories, no credential) and the installation's own routines (`NAME.md`)
in one read-only directory of the dreamer's pod, `/etc/agent/definitions`
(`dreamer.definitions`, on by default). `cmem routine` lets a restricted
harness read the directories in `CMEM_ROUTINE_READ` and nothing else.

**The dreamer may be given tools to check a claim.** As an agent is given MCP
servers, the dreamer is (`dreamer.mcp`), with a list of the tools of theirs
it may call (`dreamer.mcpTools`), read-only ones, named by the installation.
`cmem routine` passes exactly those to the harness, and no other server.
"You are not a source" stays, refined: where an entry's claim about a ticket
or a document can be checked with a tool it was given, the routine checks
it, acts on what it saw, and says in the reason what it checked and when.
With no such tool, it stays a question for the owner.

**A note can add to an entry without correcting it.** `cmem note --adds-to
ID` stores the note beside the entry, as `--amends` does, and the entry
points at it ("Added to by NOTE: ..."), marked "added to": not corrected,
not disputed, and not sorted after other entries as a disputed one is. A
short entry and a note that adds to it, equally backed, may be merged by
consolidation into one entry with nothing dropped.

The relation is kept as data on the flag note that is the pointer
(`relation: amends|adds`, `note: ID`); a pointer written before this is
still known by how its reason begins. The consolidation role can turn a
"corrected in part" mark into "added to" when the owner says nothing was
corrected: `cmem relabel ID --run RUN [--note NOTE] REASON`. The relabel is a
note of its own that names the flag, belongs to the run, counts against its
cap, is in its log, and is undone when the run is reverted. No note is
edited. The routine does this only on the owner's word; where a "correction"
changes nothing the entry says, it asks.

**A restricted agent knows its limits.** Whenever the harness is started
with only the memory command and what is above (by `cmem routine`, and by
`agent-resume --hold` from the same flags, `cmem routine --flags`), it runs
in `dontAsk` mode with Bash, Read, Glob and Grep and no other built-in tool,
in an empty working directory, and is told in an appended system prompt what
it may run, read and call; that a shell call must hold memory commands only;
and that a refused call is that call alone.

What it is told about joined commands was measured, with a stand-in for the
memory command and the cheapest model, on Claude Code 2.1.323 (the agent
image) and 2.1.326, under `Bash(cmem *)`:

| Ran | Refused, and nothing in it ran |
|---|---|
| `cmem A; cmem B`, `cmem A && cmem B`, `cmem A \|\| cmem B`, two lines | `cmem A > FILE` |
| `cmem A \| head`, `\| grep`, `\| tail`, `\| sed -n`, `\| wc -l` | `for id in A B; do cmem get $id; done` |
| `cmem A 2>&1`, `cmem A 2>/dev/null` | `cmem recall "$(date)"` |
| quoted text with `;`, `&`, `\|`, brackets | `cd /tmp && cmem A`, `NAME=x cmem A` |
| `echo`, `which`, `pwd`, `ls` and `head` inside the allowed directories | `cat`, `ls` or `Read` outside them |

The harness allows some read-only commands of its own beside the rule; the
prompt does not offer them, and says only what is safe to rely on. A long
output is kept by the harness in a file of its own and read back with Read
or Grep, which is why those stay even when nothing else is to be read. An
MCP tool not on the list was refused while one on it ran.

## Consequences

- The memory a session starts with should stop gathering copies of job
  instructions and snapshots of state. What is there already is for the
  dreamer to retire or ask about, a run at a time.
- The proxy's credential for a server is the same whichever pod calls it
  (ADR 0031), and a tracker's credential can usually write. What keeps the
  dreamer to reading is the harness's list of tools it may call, not the
  credential. A harness that failed to apply its allow-list would let the
  dreamer write with it.
- The dreamer reads every agent's settings. They hold no credential, but
  they say what each agent may reach.
- `agents.yaml` is made at deploy, from the values; a routine changed in the
  cluster since (`deploy.sh CONTEXT routine FILE`) is seen at once, an agent
  added to the values only after a deploy.
- The relabel is the first change a run makes that adds, supersedes or
  retires no entry. It counts as one against the run's cap.
- The session-start text grows by a sentence that every session pays for.
  Whether sessions follow it is to be seen in what they record, and is not
  measured.
- A service older than this does not know `--adds-to`'s relation: it stores
  the pointer as a flag that reads as wrong. Client and service come from the
  same image, so a deploy brings both.
- Added 2026-11-25: a restricted routine may also be given command
  prefixes that only read (`dreamer.commands`, `CMEM_ROUTINE_COMMANDS`),
  each an allow rule `Bash(PREFIX *)` and named in what it is told
  (ADR 0046, proposed).

## Alternatives considered

- **A header on the adding note (`adds: ID`)**, as ADR 0025 considered for
  amendments. Cleaner to read, but it changes the entry note's format and
  every place an entry is shown, and a relabel would then have to edit or
  replace the note. A pointer that is a note of its own keeps both notes as
  they were written.
- **Letting the dreamer decide a correction is an addition by reading the
  two.** It might often be right, but "nothing was corrected" is a claim
  about the world. It asks.
- **Giving the dreamer every tool, its limits around the pod, as agents
  have.** Its job is to change memory unattended; anything it can reach it
  might change on a misreading. Reading named directories and calling named
  tools is enough for the checks wanted.
- **Not telling the agent its limits, and relying on the refusal message.**
  The message `dontAsk` gives does not say which part of a call was refused,
  and the resumed dreamer read it as losing its shell.
