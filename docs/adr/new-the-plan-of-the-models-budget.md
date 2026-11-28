# new-the-plan-of-the-models-budget: The dispatcher plans which account the proxy holds, by arithmetic on the readings, and the room it finds is divided between the owner's priorities

Status: Proposed (2026-11-28). The owner: "budget planner doesn't 'build'
on switching, it drives switching to maximize output ... take into account
the 5-hour, 7-day, and [the strongest model's] windows for both accounts,
and the fact that they don't reset simultaneously nor evenly spread through
the week", and on what it is for: "how do we feed in the priorities? how
does it decide what to work on, which models to use ... ideally i want to
be able to divide the available usage between priorities too". Adds to ADR
0055 (several accounts, the proxy holds one), 0060 (pacing to a line) and
0064 (usage and budget at the proxy).

## Context

An installation may have several accounts for the model (ADR 0055). Each
has three limits that matter: a five-hour window, a seven-day window, and
a weekly window of one model alone (the strongest). The proxy holds one
account, and a person switches it. Nothing said when to.

Measured on the readings of two accounts over three days of heavy use:

- One full five-hour window costs 13 to 16% of the seven-day window and 21
  to 25% of the strongest model's own. A week has 33 five-hour windows; an
  account's week holds about 7 of them full, about 4 on the strongest
  model. The weekly windows bind. The five-hour window is a rate limit:
  it decides how fast a week can be spent, not how much there is.
- The strongest model's own week runs out first: at its end the seven-day
  window stands at 60 to 70%, and the rest is for other models only.
- The accounts' weeks end on different days. Room is lost in one way: an
  account's week ends with room while the proxy holds another. Twice in
  those three days the proxy held the account with a week ahead of it
  while the other's week was about to end half used.
- Only the held account is read (from the headers of its answers). Of the
  other, the last reading stands: its weekly windows move only by what the
  owner's own sessions use on it, and its five-hour window is empty once
  its reset has passed.

## Decision

### The plan is arithmetic, in the dispatcher

No model decides which account is held. The dispatcher computes a plan
from `usage.observed` and `model.account` in its log; `dispatcher plan`
shows it, and with `dispatcher.plan.enabled` the dispatcher records
`budget.plan` whenever the account the plan would hold, or the rule that
says so, changes. This part switches nothing.

**Each account's standing.** A reading is of the account the latest
`model.account` before it names. The readings taken between a switch and
its record are the new account's; they are known by their weekly reset,
which is the next account's and not whole weeks from this one's. A window
whose reset has passed since it was read is empty.

**What a window costs.** For each account, the rise of the seven-day
window, and of a model's own, per rise of the five-hour window, summed
over consecutive readings of the same windows. Measured over less than
half a five-hour window it is not trusted and the defaults stand (14% and
23%). This is the cost of the mix of models that ran. What each model
costs alone is not in the readings; it needs the meter's count of tokens
by model set beside them, which is the next thing to measure.

**The rules**, in the order they are applied. A *window* below is one
full five-hour window of work.

1. *Room.* Of a weekly window, for the agents: 1 − used − reserve × (time
   left ÷ a week). The reserve (`dispatcher.plan.reserve`, a quarter) is
   the owner's own sessions'. In the last three hours before the reset it
   is nothing: what he has not used is spent, not lost.
2. *Windows.* Room of the seven-day window ÷ its cost; on a model with a
   week of its own, the smaller of that and its own room ÷ its own cost.
3. *Which account.* Urgency is windows ÷ hours to the account's weekly
   reset. Among the accounts whose five-hour window has room (rule 4),
   those that admit work that can wait (rule 5) come first, and of them
   the most urgent is held. A switch needs the other to be a fifth more
   urgent, and half an hour since the last one.
4. *The five-hour window.* The agents stop at 85% of it
   (`fiveHourCeiling`); the rest is the owner's. At that the proxy goes to
   another account whose window has room, and comes back by rule 3. With
   none, only work that cannot wait runs until a reset.
5. *Pace.* Work that can wait runs on an account while its seven-day
   window is at or under a straight line from nothing at the week's start
   to 1 − reserve at its reset (ADR 0060's line, with the reserve), and
   in the last day before a reset with room left. The held account is
   worked until it is five points ahead of its line, so that two accounts
   on their lines are not switched between at every look.
6. *The model.* Work on a model with a week of its own runs while that
   week has room and is under its own line; otherwise a tier down, on
   another account, or it waits.

**The days ahead.** The plan walks the next seven days by these rules in
steps of ten minutes, with work always waiting, at one window in five
hours: for each day and each weekly reset, how long each account is held,
how many windows it is worked, how many of them on the model whose own
week is tightest, and each switch with its rule. It says what each account
loses at its reset if the proxy is elsewhere until then, and what the plan
itself leaves.

Candidates not chosen for which account is held:

- *Switch when refused* (what ADR 0055 left open). It loses nothing of
  the five-hour window and everything of the week: by the time an account
  is refused, the other's week may have ended.
- *Earliest reset first.* Right when the account whose week ends first
  has room, wrong when it is ahead of its line and the other is behind.
  Urgency is the same idea with the room counted.
- *A model decides.* The question has one answer given the readings, and
  a person has to be able to check it by hand.

### The room is divided between priorities

The windows are the easy half. What the owner asked for is to say what
the usage is for. Of the six points below, the first two are built: the
priorities, their shares and their lists are kept (`dispatcher priority`,
`internal/dispatch/budget`), and `dispatcher plan` divides each account's
room by them. Nothing yet carries a priority to the meter, picks an item
or chooses a model.

1. **Priorities are the owner's.** A priority is a name, a share of the
   usage, and under it what he wants done, in order, or "you choose". He
   says them in plain words to a session on his own machine, at any
   time; it writes them down as events (`budget.priority`: a name, a
   share, his words on it; `budget.item`: something to be done under
   one, marked hard or with the model it must run on), as the list of
   improvements is kept (ADR 0058). A priority's part is its share of
   the sum of all, so shares need not add up to anything. Only the owner
   and his machine set a priority, add, move or drop an item; an agent in
   its pod reads them and may say that it started or finished an item.
   One priority is the default.
2. **Shares divide the room.** The plan's room, per account and
   separately on a model with a week of its own, after the owner's
   reserve, is split by the shares: `dispatcher plan` says for each
   priority how many windows it has on each account until that account's
   weekly reset, how many of them on the model whose own week is
   tightest, and the next items under it. A priority's standing, its
   share of the week so far against what it spent, needs point 3.
3. **Work carries its priority.** From the ask (a task's class, a
   message, a session opened) to the meter's record, so that what a
   priority spent is measured. What exists: a task has a class and a
   priority number, which admission reads; the meter records by agent and
   pod (`model.usage`), and by model; a run's cost is `run.usage`. What is
   missing: a priority on a message and on a session (so on the pod's
   use while that session works), and on a standing pod with several
   sessions the meter cannot tell which session called: either one
   priority per standing agent, or the session's hooks say which priority
   is at work now and the meter's record is split by time.
4. **Within a priority, in order.** The planner takes the next item,
   estimates it from the record of runs like it (`run.usage` by class),
   and chooses the model: the cheapest tier with a record of doing that
   kind of work; the strongest only where the item is marked hard or a
   tier below failed at it; a tier down when the strongest model's share
   is spent. "You choose" is the one question given to an agent with a
   model: which of the agreed items is worth the room.
5. **Share is not lost.** What a priority will not use before a reset
   goes to the next priority's work. The owner's own asks are never held;
   they count against the priority he names, or the default.
6. **He sees it.** Per priority: its share, what it spent, what ran on
   which model, what is next, what was held and why.

### What switches the account (not built; the owner's choice)

Nothing here switches the installation's account, and nothing will until
the owner says so. Two ways, his to choose: both accounts' credentials at
the proxy's meter, which then also reads the one not held with one request
an hour to the cheapest model (it starts that account's five-hour window,
which the plan can use); or the switch run on the owner's machine, where
the credentials are and a watcher runs (ADR 0076), which works only while
that machine is awake.

## Consequences

- The owner, or a session of his, can ask at any time which account the
  proxy should hold and why, and what the other choice loses, and check
  the answer against six rules.
- The plan is as good as the readings. An account never held since the
  log began is not known; one not held for days is known as it was, less
  what the owner used on it since.
- The cost of a window is of the mix that ran. A week of cheap models
  after one of the strongest moves the plan's figures.
- The simulation takes work as always waiting. It says what the accounts
  can give, not what will be asked of them; with priorities it will be
  fed by their lists.
- Two rules now draw a line: ADR 0060's admission (to its target) and
  this plan's (to 1 − reserve). When admission by the plan is built they
  become one.

## The check

`cmd/dispatcher/plan_test.go`: two accounts as they were read over a day
(one late in its week and half used, one a day into its week), with the
readings between a switch and its record; each account's standing and
costs, rule 3's choice and what leaving the other account loses, rule 4
and the margins, the days ahead (the week that ends first is spent before
it ends, nothing above three points is left at a reset, the long week is
not spent in its first days), and that the advice is recorded once and
again when it changes; and the room divided by the priorities' parts.
`internal/dispatch/budget`: shares and parts, the default, items in order,
and who may do what; `internal/dispatch/api`: the same through the API as
the owner, his machine and a caller that is no agent's pod.
