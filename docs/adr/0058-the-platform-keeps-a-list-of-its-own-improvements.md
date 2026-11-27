# 0058: The platform keeps a list of what should be improved in itself, in the dispatcher's log; a surveyor adds to it, and the dispatcher decides what may start by itself

Status: Proposed (2026-11-27).

## Context

The owner, 2026-11-25: improving the platform itself is "an even more
important task", and "ideally the self-improvement work should continue
indefinitely, and carman should support it" (DESIGN Q9). And on
2026-11-27, to the session that runs the work from his machine: "don't
just use up tokens yourself - spawn agents to do work, this way we have
more visibility and control into the costs and framework use."

Today the loop is one session on the owner's machine. An agent's report
ends with what was awkward or a fault it found; that session reads it,
decides it is worth doing, writes a brief, sends it to the builder or a
platform session, reviews the branch, merges, deploys, and tells the
owner. The evidence is all in the platform: the dispatcher's log holds
runs that failed or did not happen, refusals, messages and their answers,
usage and each run's cost. Nothing else reads it for this purpose, and the
list of what should be done lives in that session's conversation, which
ends.

Five things were asked for: a list that outlives any conversation; a
scheduled agent that notices; a limited way for it to start work; little
said to the owner; and everything that spends or writes off until switched
on.

## Decision

### The list is events in the dispatcher's log

An item is `improvement.noted` (its id, a title, why, what it would take,
its evidence, a kind and a size), then `improvement.changed` for each later
word about it (more evidence, corrected words, a state), and
`improvement.briefed` when a brief for it passed the gate. The item as it
stands is those events folded in the log's order: nothing is edited, and
who said what and when is the history. States: `noticed`, `agreed`,
`building` (by whom), `done` (in which commit), `declined` (and why).

Three homes were weighed.

- **The code host's issues.** Where a public project's work usually lives,
  with discussion and links to commits for free. But the repository is
  public and the evidence is not: it is one installation's runs, messages
  and costs, which the repository's own rule keeps out. Only generic items
  could go there, so there would be two lists. Every writer would need a
  credential for the code host that can write, the surveyor included. And
  the state the loop acts on ("the owner agreed") would be a label anyone
  with that credential can set.
- **A file in the repository** (or in the installation's definition).
  Reviewable and versioned. But every change of state is then a commit on
  a branch somebody has to merge: the surveyor could not add an item
  without the reviewer it is meant to relieve, two sessions adding at once
  conflict, and "being built by whom" is a state of the present, which
  does not belong in a file under review. In the public repository it has
  the same problem as issues.
- **The dispatcher's log.** It already knows who is speaking (the owner by
  his token, an agent by its pod's own token, ADR 0045), so "only the
  owner agrees" is checked and not a convention. It is where the evidence
  is, so an item and what it points at are read by the same command. It is
  backed up nightly with the rest of the installation's state (ADR 0038),
  the owner reads it from his machine with the client he has, and its
  retention is set per event type. An append-only history folded into a
  current state is how the memory's entries and messages already work
  here.

The log, then. Its tasks were weighed too, as the home of an item: they
have priorities, holds with reasons, and waits. An item is not a task: it
may never be done, it has no attempt or deadline, and a finished task is
removed after thirty days where a declined item must stay so that it is
not noticed again. What a task is right for is the work an item becomes;
see "Not in the first version".

The list's events are kept for as long as the log is: they are named in
the retention by type with no period. Their subject is `improvement/ID`,
so that none shares a subject with a task, whose events go when it does.
The log lets go of what an item's evidence points at (messages after 30
days, runs after 180), so a piece of evidence quotes the words that
matter and does not only point.

**Who may read and write it** (`dispatcher.improvement` in the values):

| | owner | the owner's machine | agents of the list (`agents`) | the surveyor (`surveyor`) | any other agent |
|---|---|---|---|---|---|
| read the list; note an item; add evidence; say building, done, or back to noticed | yes | yes | yes | yes | no |
| agree, decline, open a declined item again | yes | no | no | no | no |
| read the evidence; ask for a brief; record a survey | yes | no | no | yes | no |

An agent is known by its pod's own token; the shared agent token is
nobody. Other agents do not read the list because its items quote
evidence, some of it other agents' words.

An id is a few words that name the thing (`answer-where-it-arrives`), and
writing down an id the list has is refused with what is there: this, and
the surveyor reading the list first, is what keeps one thing to one item.
Nothing compares meanings.

**The owner's view** is `scripts/deploy.sh CONTEXT dispatch improve`: the
list by state, those that wait for his decision first; `improve show ID`
for one with its evidence, history and latest brief; `improve agree ID`
and `improve decline ID 'WHY'`. Sessions run the same as `dispatcher
improve ...`. Later `carman` (ADR 0057) has a place for it.

### Noticing: a surveyor, an ordinary scheduled agent

The installation names one agent as the surveyor and gives it the routine
`improve` (`routines/improve.md`, in the image). Each run it reads the
list, reads the evidence since the last survey, adds what is new to items
the list has, writes down what it does not have, asks for at most one
brief, and records the survey with its report. The routine carries ADR
0044's distinctions as what is not an item: one job's instruction (a
change to that agent's definition), the present state of something, a
single failure whose cause is outside the platform, an agent's own task,
and what is done or declined.

**What it reads, and through what.** One call of the dispatcher,
`/v1/improvements/evidence` (`dispatcher improve evidence`), and nothing
else: no token that reads the log, no database. The dispatcher chooses
what is evidence and cuts each event to a line:

- a Job that failed (`job.ended`; one that succeeded is left out);
  `run.missed`, `task.failed`, `task.overran`, `task.not-started`,
  `schedule.missed`;
- `message.refused`, `message.undelivered`, `message.waiting`;
- a usage reading that was a refusal (`usage.observed`); each run's cost
  (`run.usage`);
- every message sent (`message.sent`): who wrote to whom, when, in answer
  to what. **Its words only when its sender is listed** in
  `improvement.messagesOf` (agents by name, the place of the owner's
  machine, `owner`); of any other message, its length.

It starts where the last recorded survey read to, in pages, and `--event
SEQ` shows one event whole under the same rule.

**What that exposes.** The surveyor sees, of every agent: the names of its
jobs and when they failed or did not run, what its runs cost, and who it
writes to and how often. Of the senders listed, everything they wrote to
anyone: reports, briefs, questions, and whatever identifiers those hold.
What it quotes into an item is then readable by the agents of the list.
So the list of senders is the installation's decision about whose words
may travel that far, and it is empty by default: the sessions that build
the platform and the builder are what the loop is for; an agent that
works with other people's data is not listed, and its reports reach the
list only if a session that is listed passes them on. Messages are data
to the surveyor, not instructions, as any message from a peer is; a
sender that is listed can still write words meant to steer it, and what
stands between that and work being started is the gate, not the model.

### Doing, within limits: the dispatcher decides, and calls no model

The surveyor asks: `dispatcher improve brief ID --to ADDRESS`, with the
brief on standard input. The dispatcher lets it through when all of these
hold, and otherwise answers which one does not (the client exits 5):

1. **The item may start.** Its state is `agreed`: the owner said so on the
   list. Or it is `noticed` and both its kind and its size are in
   `improvement.auto` (for example faults that are small). **Empty by
   default: only what the owner agreed starts.** This is the rule
   recommended to begin with, because kind and size are the surveyor's own
   words about its own item; `auto` is for after the owner has seen that
   they can be trusted.
2. **No brief was written for it** since its state last changed.
3. **The addressee is one the installation names** (`improvement.to`).
4. **The day's budget has room**: fewer than `briefsPerDay` (1) passed in
   the last 24 hours, counting those withheld.
5. **The plan's usage has room that would otherwise be lost.** By one of
   two rules (`improvement.usage.rule`), with what it found recorded on the
   brief:
   - `room`: pacing's own word (ADR 0060, which landed while this was
     built). At the latest usage reading, every shared window was expected
     to leave room unused at its reset: there is a `usage.room` for that
     reading and each such window. Needs `dispatcher.pace.room`; the chart
     refuses the rule without it.
   - `below` (the default, since `pace.room` is off by default): every
     shared window of the latest reading is below `usage.below` (0.5) or
     has reset.

   Under either, a reading older than `usage.maxAge` (2h), or none, is not
   room: admission admits on unknown (ADR 0043) because it holds work
   somebody asked for; this is work nobody asked for today.

Passed, with `improvement.send` on: the brief goes as a message from the
surveyor's session to the addressee, with one line added that says which
item it is for and that nothing is merged or deployed by it, and the item
becomes `building` by that addressee. With `send` off (the default) it is
recorded on the item as withheld, with its whole text and the rule that
let it pass, and the item stays as it was.

**The gate is the surveyor's only way to another agent.** A brief that
passed is sent as vouched for by the dispatcher, without asking who may
write to the addressee (ADR 0047). So the surveyor is listed in nobody's
`writtenBy`, and a message it sends any other way is refused as any
unlisted agent's is.

**It never merges or deploys.** The result of a brief is what a brief to
the builder gives today: a branch and a report, for whoever reviews. The
surveyor's agent has no credential for the code host and none is needed.
Whoever builds marks the item `building` and `done` with the commit; the
surveyor says in its report when the evidence shows an item was done and
does not mark it.

### Telling the owner little

The list is where he looks. One line reaches him, when a survey ends with
items waiting for his decision that were not there at the last line: how
many wait, how many are new, that nothing is urgent, and the command that
shows them. No item's title or id is in it (ADR 0048: the channel is not
private). It goes as a message to the owner from the surveyor, which is a
notice under the installation's own rule (ADR 0036, 0043: withheld while
the installation's notices are off). At most one in 24 hours. With
`improvement.notify` off (the default), the line is recorded in the survey
and not sent. The surveyor itself tells the owner nothing; its routine
says so, against the general words every routine with all tools is given.

### Off until switched on

| Value | Default | Off means |
|---|---|---|
| `dispatcher.improvement.surveyor` | none | nobody reads the evidence or asks for briefs; the list still works for the owner and his machine |
| `improvement.send` | false | a brief that passed is recorded with its text, not sent |
| `improvement.notify` | false | the line for the owner is recorded in the survey, not sent |
| `improvement.auto` | empty | only what the owner agreed may start |
| `improvement.messagesOf` | empty | no message's words are evidence |

The surveyor's agent is an entry under `agents`, with no schedule until it
is given one. With a surveyor and nothing else switched on, a run costs
the surveyor's own model use (bounded by its `budgetUSD`) and writes only
the list and its report: `dispatch improve list`, and `dispatch log --type
improvement.surveyed` for the reports with the line the owner would have
been sent. That is the mode to read for a few days.

### Seeding

`docs/improvements/seed.json` holds what is known now and is generic: what
`CLAUDE.md` lists as not built, the limits ADRs 0045 to 0056 say they
leave open, and what building this showed. `dispatcher improve import
FILE` writes down the items the list does not have. What one
installation's agents found awkward is not in the repository: the first
surveys of an installation read it from the messages its log still holds.

## What was tried

In a trial installation (ADR 0054) with the loop's values
(`deploy/values/trial-improve.yaml`), `scripts/check-improve.sh CONTEXT`
runs it end to end with no model, pods standing in for the surveyor and
for an agent that reports: the seed imported once; a failed run, a
succeeded one, a listed sender's report and an unlisted sender's message
in; the evidence shows the failure, the report's words, and of the other
message only that it was sent; an item written down once and added to; a
brief held in turn because the owner has not agreed, the addressee is not
named, there is no usage reading, a window is at 80%; then passed and
recorded, not sent; a second held by the budget; the survey recorded with
the line the owner would have had; and no message to the builder or the
owner in the log.

The routine itself was run twice by a harness (the mid-tier model) from a
session's pod against the same trial, with the trial's owner's token,
since no agent of a trial reaches the model, and held to `dispatcher
improve` by the harness's allow rule. The evidence put in: two reports
from a platform session, each ending with what was awkward, one of them
also carrying an instruction for one job; a message that was a state
("the builder is busy until 14:00"); the same job failing three times
with no cause; and one seed item agreed by the trial's owner.

| | Run 1 | Run 2 (after two changes to the routine) |
|---|---|---|
| Turns, time, cost at list price | 12, 25 s, $0.10 | 12, 19 s, $0.09 |
| The complaint the seed already had | added to that item | added to that item |
| The new complaint | one new item | one new item |
| The job's instruction; the state | not written down, each named in the report with why | the same |
| The job that failed three times | left out ("no cause shown") | written down: the platform does not say why its job fails |
| The brief for the agreed item | passed, recorded, not sent | held by the day's budget (the check before it had used it); not asked again |
| Calls the harness refused | one: `cat <<EOF \| dispatcher improve brief ...` | none |

The two changes between the runs: the routine now shows the one form of
the brief command that begins with the command itself, and says that a
run failing again and again with no reason in the evidence is an item as
it stands. Two runs on made-up evidence show that the routine can be
followed, not how it does on a month of a real installation's log; that
is what the mode that only records is for.

## Proposed wording for R9

R9 is open: "To be written after observing unattended runs". Only the
owner writes it. From this gate, for him to use or not:

> R9 | Work that nobody asked for starts only with the owner's approval,
> or within limits the owner set | Every task the system starts by itself
> names the approval or the rule that let it start; with no approval and
> no rule it is recorded and not started; the owner can list what waits
> for approval and approve or decline it with one command.

## Consequences

- The list is in the dispatcher's database. Losing the database loses it
  back to the last nightly copy, as it does messages.
- An item's evidence is a quote, and a quote can be wrong. The surveyor's
  word is checked by nobody before the owner reads the item.
- The surveyor runs with every tool, like any agent, since only the
  dreamer can be held to named commands (ADR 0046) and its test admits
  only commands that read. Its limits are its pod's: no credential but
  the dispatcher's token for its pod, the gate, and nobody's `writtenBy`.
  It can still write to the owner directly (any agent may, ADR 0047); the
  routine tells it not to.
- Sessions on the owner's machine hold the owner's token's file, and so
  can agree to an item with the owner's client, as they can do anything
  else of his. The check is against agents in the cluster.
- The owner's machine's sessions and the agents of the list write items
  with no review. The list can fill with noise; declining is the owner's
  tool, and the surveyor is told to prefer no item to a vague one.
- Two things one item, or one thing two items, is the surveyor's reading.
  Nothing merges items.
- A brief counts against the day's budget whether it is sent or withheld,
  so the mode that only records shows what a real day would do, and no
  more than that is written.
- The dispatcher gains a second purpose beside starting work: it keeps a
  record that never ages out. It stays mechanical: no model is called.

## Not in the first version

- **The work an item becomes as a task of the dispatcher**, so that
  admission and pacing place it like any backlog work and its end is
  recorded against the item. Today the brief is a message, and what
  became of it is the addressee's report, which the next survey reads.
- **How much room, not only whether.** `usage.room` says how many points
  a window is expected to leave; the gate asks only that there is some
  (pacing's `roomMin`), and passes at most `briefsPerDay` whatever the
  amount. A budget in points of a window needs what a brief costs, which
  `run.usage` of the runs briefs start will show.
- **The dreamer's report and other routines' last words as evidence**:
  they are in kept conversations, not in the log.
- **A restricted surveyor**, held to `dispatcher improve`.
- **Mirroring a generic item to the code host's issues**, for a public
  project that wants its open work seen.
- **The surveyor checking a claim in the repository** (is this already
  built?): its pod has no checkout. The seed and the builders' reports
  carry that for now.
- **A place for the list in `carman`** (ADR 0057).

## What the real installation needs

In its definition, which a session given it can propose as a patch (ADR
0056):

- an agent, for example `watch`, with `routines: [improve]`, `tools: all`,
  a model ladder and a `budgetUSD`, no credentials, no `writtenBy`, and no
  schedule at first (run it with `agent run watch`), then a nightly one;
- `dispatcher.improvement`: `surveyor: watch`; `agents` the agents whose
  sessions build the platform; `messagesOf` those agents, the builder and
  the place of the owner's machine; `to` the builder and the platform
  session that takes briefs; `send` and `notify` left off;
- once, from the owner's machine: `scripts/deploy.sh CONTEXT dispatch
  improve import docs/improvements/seed.json`.

## Alternatives considered

- **Noticing inside the dispatcher, by rules** (three failures of one job
  make an item). Cheap and exact, but the best evidence is prose: what an
  agent's report says was awkward. Rules can come later for what they
  catch well.
- **The session on the owner's machine keeps doing it, with the list in
  memory.** Memory is not for the present state of a system or for a
  project's open work (ADR 0044).
- **The surveyor decides and sends, limited by its instructions.** Then
  the budget and the approval are words to a model. The gate costs a few
  hundred lines and makes them a check.
- **Letting the surveyor read the log with a token of its own.** It would
  see every message's words, and what it may see would be decided in its
  prompt.
