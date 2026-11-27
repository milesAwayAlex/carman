# new-task-stream-benchmark: The task-stream benchmark is a made world worked through over days, in trials, scored by the world and not by a model

Status: Proposed (2026-11-27). The design and its cost; nothing is built or
run until the owner's side says so.

## Context

M7: "On the benchmark task stream, with the same model and harness, tasks
run with carman succeed at least as often as with Claude Code's built-in
memory, at a cost per task within a margin the owner sets." R8 asks the
same of consolidated memory against raw. The task stream is the third
piece of the memory benchmark (ADR 0013) and the only one not built.

**What the earlier benchmarks showed, which this one does not repeat.**

- `bench/retrieval`: search and flashback find a note whose right answer is
  known, with no model. The stream does not measure ranking.
- `bench/transfer` (`RESULTS.md`): a lesson a person gives by a correction
  in one session is used in a later one, 9 of 9 with cmem and its hook, in
  the same repository and another; built-in memory carries it within a
  repository about half the time and across repositories only from a
  shared directory; a note's scope, a lesson that changes, a long entry
  corrected in one fact, a memory larger than its index, and the tidying
  routine on a planted mess all behave. Each case is two or three short
  sessions, the lesson is said in so many words, and the memory holds a
  handful of entries the test put there.
- `bench/clbench`: the pilot was inconclusive. The task was noisy, and the
  arms' instructions were worded differently, which alone moved the
  scores. And on the cheapest model the agent accepts a correction and
  writes nothing down: there the limit is the writing, not the store.

What none of them shows is the claim itself: that an agent **working**
through many tasks over days does better with this memory than without.
That needs what the others leave out: lessons the work teaches and nobody
states; a memory that the agent's own sessions fill, tens of entries and
then more, some of them wrong or out of date; the nightly tidying acting on
that memory and not on a planted one; and the cost of carrying memory into
every session set against what it saves.

## Decision

### The stream is a made world, and the world keeps the score

**A world service.** A small program with no model (a pod of the trial)
plays the systems a working agent meets: a registry, a deploy interface, a
ticket queue, a configuration store, each reached by a command in the
agent's image. The world has rules and facts that are nowhere written down
for the agent: a flag a command needs, a region a service must go to, an
approval that has to exist first, a name that is reserved. A wrong call is
answered as a real system answers: an error that says what was wrong, or
only that it was. The rules live in the service, not in the agent's pod, so
they are learned by acting, at the cost of attempts, and not by reading the
checker.

**A task is a ticket**: a few sentences asking for a state of the world or
a file ("put the billing service's new image out to the test stage",
"write the rollback plan for…"). Every task is a fresh session of the
trial's agent, started as a job by the trial's dispatcher, with a limit on
turns and on cost. Nothing passes from one task to the next but the arm's
memory.

**Scored by the world, with no model judging.** After the session ends the
runner asks the world what state it is in and reads the files the task
names, and a script decides: pass or fail, and how many wrong calls the
world answered on the way. The agent's own account of its work is not
read. The answers are not in the agent's pod.

**What makes a later task easier.** Lessons, each a family of tasks:

1. *met by doing*: the first task of the family can be done without
   knowing the rule, by running into it (two to five wrong calls, or
   failure within the limit); later tasks of the family, the same day,
   the next, and three days on, need the same rule in a different ticket;
2. *told once*: the ticket of one early task carries a standing
   instruction from the owner ("from now on…"), which later tickets do not
   repeat: what `bench/transfer` tests, kept small here as a link to it;
3. *a rule that changes*: on a later day the world changes (the registry
   moves, the flag is renamed), a task meets the change, and tasks after
   it are scored on the new rule; following the old one is counted
   apart, as staleness;
4. *the tempting wrong answer*: a later ticket that looks different, where
   the practice that failed earlier is the obvious thing to propose;
5. *a wrong memory*: a note that is in memory from the start, marked as an
   agent's unverified observation, which the world contradicts; following
   it without checking fails the task;
6. *neutral tasks*: tickets no lesson bears on, some of them close to one,
   so that applying the lesson is the mistake; and tickets about something
   memory cannot know. Their score should not differ between arms; where
   it does, memory did harm.

Some families span two "repositories" of the world, so that a note's scope
matters (a rule of one, a rule of all).

**The size.** Five days of eight tasks: forty tasks, in one fixed order for
every arm. Twelve lesson families (six met by doing, two told, three that
change, one tempting wrong answer), with about twenty-six tasks that a
lesson bears on after its first meeting, eight neutral tasks, two with a
wrong memory, and the families' first meetings. The tasks are written and
committed before any arm with memory is run on them, and the pilot uses
tasks that are not among them.

### The arms: the same model, harness, image and wording

1. **none**: no memory; the harness's own switched off.
2. **built-in**: Claude Code's own memory directory, carried from task to
   task. M7 names it: it is what carman has to match.
3. **notes**: cmem with its hook (the index at a session's start, the
   flashback), filled by the sessions themselves; nothing tidies it.
4. **tidied**: as notes, and between each day and the next the dreamer's
   two routines run on the trial's memory, as they do nightly on a real
   one.

Each arm is a trial installation of its own (ADR 0054): its own memory
service on an empty library, its own dispatcher, its own world, an agent
that reaches the model through the meter's listener for trials (ADR 0065).
The agents are told about memory in one paragraph built from one frame, in
which only the sentence naming the mechanism differs; the arm with none
gets a paragraph of the same kind saying there is none (the first clbench
pilot's lesson). An arm that knows every lesson from the start (the
ceiling) is left out of the first run: it costs a fifth more and answers a
question nobody has asked yet.

**The model.** The run that can answer the question is on Sonnet: on the
cheapest model the agent does not write lessons down (`bench/transfer`), so
a run there would measure that again. The pilot is on the cheapest, since
it tries the harness and not memory.

### What is measured

For every task, from sources the agent does not write:

- **success**, and **wrong calls** before it, by the world;
- **cost**: tokens by kind and requests, as the meter saw them for that
  task's pod (ADR 0050, 0064), and the turns the harness reports; the
  dreamer's nights counted to the tidied arm;
- **memory's part**: from the memory service's own log, what the session
  was shown at its start and by flashback, what it recalled and read, and
  what it wrote; and whether an entry that holds the family's lesson was
  among what it was shown or read before its first call to the world that
  the lesson governs ("had it"), set beside whether that call was right
  ("used it");
- **harm**: neutral tasks failed where the arm with no memory passed; old
  rules followed after a change; the wrong memory followed;
- **the memory itself** at each day's end: how many entries, how many
  current, how many characters a session is shown.

Reported: success and cost per task by arm and by day; the same for tasks
a lesson bears on, by how many days lie between the meeting and the use;
cost per passed task; and each family's row, so that one family cannot
carry an average. Every run's transcripts and libraries are kept.

**How many runs, and what they can show.** Agents vary from run to run, so
each arm is run three times: 120 scored tasks an arm, about 78 of them
bearing on a lesson. Tasks are the same in every arm, so arms are compared
task by task. That is enough to tell apart arms that differ by fifteen
points or more in success on lesson tasks, and it is not enough to show
that two arms are equal within five. M7's "at least as often as" is
therefore answered as: carman is above built-in, or within the margin the
owner sets and not shown to be below it; if the two end closer than the
run can separate, the report says so and says how many more runs would
separate them.

### What would count as memory not helping

Written down before any run:

- on tasks a lesson bears on, the notes arm does not pass more often, nor
  with fewer wrong calls, than the arm with none, beyond what three runs
  can separate;
- the notes arm's cost per passed task is above the arm with none: what
  memory adds to every session is more than it saves;
- on neutral tasks an arm with memory passes less often than the arm with
  none: memory misled;
- after a rule changes, an arm with cmem follows the old rule more often
  than built-in does;
- the tidied arm is not at least as good as the notes arm on the last
  three days, in success or in cost: the nightly tidying does not pay
  (R8's second half);
- carman's arms pass less often than built-in beyond the owner's margin,
  or cost more per task beyond it: M7 is not met.

Any of these is a result to report as it is, with the transcripts that
show why.

### Where it runs

Inside trials on the platform, from the building session's pod:
`scripts/trial.sh` stands up each arm's trial, the world is one more pod
of it, a task is submitted to the trial's dispatcher as a task of the
agent's class with the ticket's name, and the runner waits for the job,
scores it from the world, and submits the next. A night is the dreamer's
routines run in the same trial. The meter's record of trials
(`trial/TRIAL/AGENT`) is the cost. What the trials may spend is the
installation's to set (ADR 0065): the pilot fits the present limit; the
run does not (below).

## The cost, before anything is spent

From what the meter and the runs' own records show (2026-11-27): a trial
agent's short run on the cheapest model, $0.0015 to $0.003; a routine in a
trial on Sonnet, $0.08 to $0.10; a survey over a long log on Sonnet, $0.28.
A task here is taken as twelve to fifteen requests in a small workspace:
about $0.01 on the cheapest model, $0.15 on Sonnet (between $0.10 and
$0.20), and about three times Sonnet's on Opus (the ratio the transfer and
distilling runs show).

- **The pilot**: six tasks of its own, two arms (none, notes), one run,
  one night, on the cheapest model: twelve sessions and two routines.
  About $0.15 at list price, under $0.50 if a task is three times what is
  assumed; about 220 requests, inside the 300 a day all trials may make
  now. It changes no setting.
- **A run that could answer the question**: forty tasks, four arms, three
  runs, on Sonnet: 480 sessions and twelve nights. About $75 at list price
  (between $50 and $100); about $7 on the cheapest model, which would not
  answer it; about $230 on Opus. About 7,700 requests: the limit for
  trials has to be raised for it (`egress.model.trials.requests`, and
  `models` to allow Sonnet), which is the owner's value to set.
- **Against the plan's windows**, from the one evening of readings there
  is on this account (heavy use of the two strongest models: about $41 of
  runs and several sessions took the five-hour window to 41% and the
  seven-day window to 20%): a point of the seven-day window is somewhere
  near $3 to $5 at list price, and a point of the five-hour window $1 to
  $2. The run is then 15 to 25 points of a week and, if it ran in one go,
  most of a five-hour window; spread by the dispatcher's pacing over a
  day it is ten points or so of each window it crosses. The week's reading
  forecast 65 points unused. These conversions are rough: one evening, and
  it is not known that Sonnet counts against the windows as the stronger
  models do.

## Consequences

- The world, its tasks and its scoring are this project's to write, and so
  can be fitted, without meaning to, to what cmem is good at. Against
  that: lessons come from the world's behaviour and not from sentences
  shaped like notes; the tasks are fixed before a memory arm sees them;
  built-in memory is an arm; every family is reported by itself; and the
  transcripts are kept to be read.
- It measures one harness. M8's second harness runs the notes arm with
  only the CLI when there is one.
- A made world is not the owner's work. What it can show is whether the
  mechanism pays where the answer is known; the improvement loop's
  evidence (ADR 0058) is where real work is watched.
