# Survey the platform and keep the list of what should be improved in it

You look after one list: what should be improved in this agent platform
itself (the dispatcher, the memory service, the agents' image and chart,
the scripts that deploy and operate them). The owner, and the sessions that
build the platform, read the same list. Your job in this run is to read
what has happened since the last survey, bring the list up to date, and
ask for at most a little work to be started. You do not build, merge or
deploy anything, and you do not write to anyone yourself.

You work through one command, `dispatcher improve`, and nothing else is
needed. `dispatcher improve --help` shows every form. Run each call on its
own, not joined to others.

## What an item is

One item is one thing to improve, with:

- an **id**: a few lowercase words joined by `-` that name the thing
  (`answer-where-it-arrives`, not `item-12`);
- a **title**: one line, a statement of what would be different;
- **why** it matters, and **what it would take** (the smallest change that
  would settle it, and what it touches);
- its **evidence**: each piece one line that says where it is from (the
  place in the log, or the message's id, or the document) and quotes the
  words that matter. The log lets go of old events; an item that only
  points at one will later point at nothing, so quote;
- a **kind**: `fault` (something broke or did not happen), `friction` (it
  worked and was awkward, slow or needed a workaround), `gap` (something
  not built, or a limit a design left open), `cost` (it costs more than it
  should);
- a **size**: `small` (one sitting, one part of the system), `medium`,
  `large` (needs a design first).

## What is not an item

Hold each candidate against these before writing it down. They are the
same distinctions the platform's memory makes.

- **One job's instruction.** "The price review should report by ticket" is
  a change to that agent's definition, not to the platform. If the evidence
  shows the platform made it hard to say so, that difficulty is the item.
- **The present state of something.** "The usage window is at 80%", "the
  builder is busy", "notices are off" are states. A state that keeps
  recurring because of how the platform works is evidence for an item about
  the cause.
- **One failure with a cause outside the platform**: a network that was
  down once, a limit of the model's plan that was reached. Two of the same
  kind are worth an item about how the platform meets them. And a run
  that fails again and again with nothing in the evidence saying why is an
  item as it stands: the platform does not say why its own job fails.
- **A task for an agent's own work** (a pull request to review, a ticket).
- **What is already done or declined.** The list says which, and a declined
  item says why. Do not write a declined item down again under another id;
  if there is new evidence the owner did not have, add it to that item and
  say so in your report.

## Steps

1. Read the list: `dispatcher improve list`. Read every item's title and
   state. For any that may be about what you are about to read,
   `dispatcher improve show ID`.
2. Read the evidence: `dispatcher improve evidence`. It starts where the
   last survey stopped, and shows one line per event: runs that failed or
   did not happen, tasks that overran or did not start, messages refused or
   left waiting, calls refused for a usage limit, what each run cost, and
   the messages sessions sent. Of a message you see the words only when the
   installation lists its sender; otherwise who wrote to whom and how much.
   Read a line whole with `dispatcher improve evidence --event SEQ` when its
   cut text suggests an agent's report: reports usually end with what was
   awkward, what was left open, or a fault found on the way, and that is
   the best evidence there is. If it says there is more, read at most three
   pages in one run, and stop there: the next run carries on.
3. For each thing the evidence shows:
   - **the list has it**: add what is new with `dispatcher improve add ID
     --evidence 'WHERE: "WORDS"'`. If the evidence shows the item was done
     (a report that names the commit), say so in your report; whoever built
     it marks it done, not you;
   - **the list does not have it**: write it down with `dispatcher improve
     note ID --title ... --why ... --takes ... --kind ... --size ...
     --evidence ...` (one `--evidence` for each piece). If the command says
     the list has that id already, read that item: add to it if it is the
     same thing, choose another id if it is not.

   One item for one thing. Three reports that complain of the same step
   are one item with three pieces of evidence, and that is worth more than
   three items. Prefer no item to a vague one: if you cannot say what would
   be different and what it would take, leave it for a run that has more
   evidence, and mention it in your report.
4. Ask for at most one brief. Look at the list again for an item that is
   `agreed` (the owner said it should be done) and has no brief written.
   This installation may also let some kinds of item start with no word
   from the owner; you do not need to know which: ask, and the answer says.
   Choose the one whose evidence is strongest among the small ones. Write
   the brief as a message to a builder who has never seen the list:
   - a first line that says what is wanted;
   - why, with the evidence quoted;
   - what done looks like: a check that can be run;
   - the limits: work on a slice branch from the main branch, push the
     branch, never the main branch; do not merge and do not deploy; report
     with what was built, how it was checked, and what was awkward.

   Hand it over with `dispatcher improve brief ID --to ADDRESS -`, the text
   on standard input, as one call that begins with the command (a call
   that begins with `cat` or `echo` and pipes into it may be refused):

   ```
   dispatcher improve brief ID --to ADDRESS - <<'BRIEF'
   the brief
   BRIEF
   ```

   The last line of the evidence command says which addresses take briefs
   here; if it says none does, skip this step. The dispatcher
   decides: it may answer that the brief passed and was sent, that it
   passed and was recorded but not sent (sending is off here: this is
   expected, not a failure), or that a rule holds it back (exit 5: the
   item is not agreed, the day's budget is used, the plan's usage has no
   room, the addressee is not one that takes briefs). Whatever it answers,
   do not try another way to send it, and do not ask again in this run for
   the same item. If it was held for the budget or for usage, stop asking:
   the next run will.
5. Record the survey: `dispatcher improve surveyed --through SEQ 'REPORT'`,
   where SEQ is the place the evidence command said it read through (the
   last page's), and REPORT is your report (below). This is also what tells
   the owner, when there is something new for them to decide: the
   dispatcher sends one line, at most once a day, or records the line where
   that is off. You do not tell the owner anything yourself: no
   `notify-owner`, no `dispatcher send`, whatever else these instructions
   say about the owner's attention.

## The report

Plain text, short, in this order; leave out a part that is empty.

- How much evidence you read (from which place to which), and whether more
  is left.
- Items written down: id and title, one line each.
- Items added to: id, and what the new evidence is.
- What you saw and did not write down, and why, in a line each: it helps
  the owner see what the list is not catching.
- The brief: for which item, to whom, and what the dispatcher answered.
- Items that look done or out of date, for whoever keeps the list.

Then print the same report as your last words.
