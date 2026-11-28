# 0078: The proxy's meter holds every account's credential and switches between them by two rules

Status: Proposed (2026-11-28). The owner, on where the credentials are:
both are needed at the proxy, so they go there; and on the switch: done
and working, by the rules that matter first. Changes ADR 0055 (the proxy
held one account's credential, and a person changed it) and builds on ADR
0077 (the plan).

## Context

ADR 0055 kept the accounts' credentials on the owner's machine and let a
person put one at the proxy. ADR 0077 worked out when a switch is due.
Room was lost meanwhile for want of a person at the right hour: an
account's week ended half used while the proxy held the other. A switch
that needs nobody needs every credential where the switch is made, and a
reading of the account that is not held.

## Decision

- **Every account's credential is at the proxy's meter.** The model's
  secret holds, beside the one credential it held (`token`), a key
  `account.NAME` for each account. `scripts/deploy.sh CONTEXT
  model-account --all` puts them there: each from its command in the
  installation's `model-accounts` to the cluster's API, through a pipe,
  never printed, logged, or written to a file on the way. With
  `egress.model.accounts.enabled` the meter mounts the whole secret, reads
  the files again as the cluster changes them, and logs accounts by name
  only.
- **The meter chooses which one a request goes out on**, when it arrives;
  a request under way finishes on the one it started with. Two rules:
  1. *Week.* Hold the account whose seven-day window resets soonest while
     it has room (under 98%).
  2. *Five-hour.* When that account's five-hour window is at the ceiling
     (`fiveHourCeiling`, 85%), hold another that has room in both, until
     the first one's window resets; then rule 1 again.
  Never more often than `hold` (30 minutes). An account never read is
  taken to have room: its first answer says. A window whose reset has
  passed since it was read is empty. With no account that has room, the
  meter stays where it is. Two weekly resets within half an hour of each
  other are the same, and the name decides.
- **A model with a limit of its own may go out on another account.** The
  two rules choose one account for everything. The strongest model has a
  weekly limit of its own, which is spent well before the seven-day
  window: rule 1 would keep the account while every request of that
  model is refused and the other account holds room for it. So the meter
  chooses for each request by its model (named in the request's first
  bytes). Where the held account's own limit for that model is refused or
  at 97%, as that model's answers said, its requests go out on another
  account that has room for it, in both shared windows and in its own
  limit for the model as far as known; the other models stay on the
  account held. It comes back when the limit's reset has passed, or when
  no other account has room. Each change of this is recorded once
  (`model.account.for`, its subject the model) and told to the owner in
  one line, not each request; the account held, and so the readings'
  account, does not change. A request whose model is not in its first
  bytes goes out on the account held.
- **What is not known moves nothing.** The two rules are for accounts
  whose standing is known. An account never read is no reason to leave
  one that answers: it is tried only when the account held is known to
  have no room and no known account has (rule "unknown"). The first
  version took an unread account to have room, and at its first start,
  its own readings refused, left the account whose week ended in a day
  for the other.
- **It begins from the dispatcher's log.** A meter that starts asks the
  dispatcher (`GET /v1/meter/accounts`, the meter's and the owner's) for
  the account the proxy held last and each account's standing as last
  read, a model's own limits too: the same reading of the log as the
  plan's (ADR 0077). It asks again every minute and takes what is newer
  than what it read itself, so an account it does not hold is known as
  the log has it, with windows whose reset has passed counted empty.
  Without the log it begins on the account whose credential is the
  secret's `token`, and stays there until something is known.
- **The account not held is read.** Every answer's headers give the
  standing of the account it went out on (ADR 0064). For the others the
  meter sends one request an hour (`readEvery`) to the smallest model
  (`readModel`), one token long, with the headers the latest agent's call
  named itself by, and reads that answer's headers. On the
  model's API as it is today such a request is refused (429) with nothing
  of the standing, as any request no harness wrote: the meter logs that
  the account could not be read and goes by the log. Leave `readModel`
  empty to send none. A reading says which
  account it is of (`account` in `usage.observed`); the standing, the
  admission rule and the reading job use those of the held account only,
  and the plan sorts them by what they say.
- **A switch is recorded and told.** `model.account`, from the meter, with
  the account, the one before, the rule, the reason in words, and every
  account's standing as it was. The dispatcher sends the owner one line
  for each. `dispatcher plan` lists the latest changes of account, a
  person's and the proxy's, with the proxy's reasons.
- **The plan's other rules are not applied here.** The owner's reserve of
  the week, pacing to a line, and priorities stay with the values and the
  parts that have them (ADR 0077); the meter's rules are the two above,
  with the five-hour ceiling as the owner's share of that window.

Candidates not chosen:

- *The dispatcher decides and tells the meter.* The meter sees every
  answer's headers at once and holds the credentials; a decision made
  elsewhere would wait on a record and a call back, and stop when the
  dispatcher is down.
- *The switch on the owner's machine*, where the credentials already are
  and a watcher runs. It works only while that machine is awake, which is
  not the nights the plan matters most.
- *Read the account not held with the reading job.* The job is a harness
  in a pod behind the same proxy; it would have to be told which
  credential to go out on, and the meter can ask in one request.

## A mode in which the account changes only when told (2026-11-28)

The rules were switched off on the installation the day they went on:
their own moves at a start were not in the log, readings were then put
under the wrong account, and the proxy went back and forth. Each change
of account also loses the prompt cache of every live conversation, which
the rules did not weigh. Until the owner says what he wants of them,
`egress.model.accounts.mode: manual` keeps what is sound in this record
(every account's credential at the meter, readings that say their
account) and drops the rest:

- No rule runs, nothing is read of the account not held, and no model's
  requests go elsewhere. The meter holds the account the dispatcher's log
  names as held (the latest `model.account`), asks the log every quarter
  of a minute, and at its start asks for up to half a minute before it
  passes any call, so that it does not begin on another account. Where
  the log names none, it holds the one whose credential is the secret's
  `token`.
- `dispatcher account hold NAME 'WHY'` records `model.account` with who
  asked and why; the meter follows. It is the owner's, a session's on the
  owner's machine, and the agents' the installation names
  (`egress.model.accounts.agents`): a pod needs no right over the secret,
  since no credential moves. A name the meter has no credential for is
  refused (the meter says which it has each time it asks the log), and so
  is a change before the meter has said so.
- The command says that every live conversation loses its prompt cache
  and names the sessions in the middle of a turn; with any, it goes
  ahead only with `--now`. `dispatcher account` says which account the
  log names, since when, who said so and why, and which one the meter
  holds.

`scripts/check-account.sh CONTEXT` tries it in a trial with two made-up
accounts and two agents, one of them named.

## Consequences

- A second credential is in the cluster's secret, readable by whoever can
  read that secret and by the meter's container, as the first is.
- While it is on, a person does not choose the account: `model-account
  NAME` changes the secret's `token`, which the meter only begins on.
  To hold one account by hand, switch `egress.model.accounts.enabled` off.
- A switch within the least time is not made, so an account refused for
  a limit just after a switch stays held for up to half an hour.
- Reading an account starts its five-hour window if none is open.
- The account not held is known only as it was when last held, less
  nothing: what the owner's own sessions use on it meanwhile is not seen
  until the proxy holds it again. A reading of it by a real harness run
  sent out on a named account is not built.

## The checks

`internal/dispatch/meter/accounts_test.go`: the two rules, the least time
between switches, the reading of the account not held, and the files.
One model sent elsewhere at 97% and at a refusal, the others staying,
told once, and back after the reset, are in the same tests.
`scripts/check-switch.sh CONTEXT`: first as a real installation starts,
the meter's readings refused with nothing of the standing and the log
empty: it stays on the account it was given; then the log is given the
other account's standing and it decides from that. Then a trial with its own proxy and meter,
two made-up accounts on the stand-in each with a standing of its own; the
meter goes to the account whose week is made to end sooner, the owner is
told, the next call goes out on that credential; at 90% of its five-hour
window it goes to the other, and returns when the window is empty; no
two switches closer than the least time; with one model's own limit at
98% on the held account that model's requests go out on the other and
another model's stay, recorded and told once; readings say their account, and
`dispatch plan` shows what the proxy did and why.
