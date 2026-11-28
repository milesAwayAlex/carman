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
- **It begins on the account a person had put there**: the one whose
  credential is the secret's `token`. The rules take over at once. A
  meter that restarts begins there again; which account is held is not
  kept anywhere else.
- **The account not held is read.** Every answer's headers give the
  standing of the account it went out on (ADR 0064). For the others the
  meter sends one request an hour (`readEvery`) to the smallest model
  (`readModel`), one token long, with the headers the latest agent's call
  named itself by, and reads that answer's headers. A reading says which
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

## Consequences

- A second credential is in the cluster's secret, readable by whoever can
  read that secret and by the meter's container, as the first is.
- While it is on, a person does not choose the account: `model-account
  NAME` changes the secret's `token`, which the meter only begins on.
  To hold one account by hand, switch `egress.model.accounts.enabled` off.
- A switch within the least time is not made, so an account refused for
  a limit just after a switch stays held for up to half an hour.
- Reading an account starts its five-hour window if none is open.
- The request the meter reads with was tried against the stand-in only.
  If the model's API refuses it without saying the standing, the meter
  logs that the account's standing could not be read, and the account is
  known only from its own answers while held.

## The checks

`internal/dispatch/meter/accounts_test.go`: the two rules, the least time
between switches, the reading of the account not held, and the files.
`scripts/check-switch.sh CONTEXT`: a trial with its own proxy and meter,
two made-up accounts on the stand-in each with a standing of its own; the
meter goes to the account whose week is made to end sooner, the owner is
told, the next call goes out on that credential; at 90% of its five-hour
window it goes to the other, and returns when the window is empty; no
two switches closer than the least time; readings say their account, and
`dispatch plan` shows what the proxy did and why.
