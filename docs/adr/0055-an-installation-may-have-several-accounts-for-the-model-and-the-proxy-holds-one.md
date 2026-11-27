# 0055: An installation may have several accounts for the model, and the proxy holds one

Status: Proposed (2026-11-27). The owner: "let's own an option to swap the
tokens ... we'll have two secrets on the keychain and we need to have a way
to swap the one the proxy is using when we need to". Adds to ADR 0050.

## Context

The model's credential is held at the egress proxy (ADR 0050): one secret,
one account. An account has limits of its own, some for one model only
(ADR 0052). When a limit is reached, every agent that needs that model
stops until the limit resets, however much another account of the owner's
has left. On the day this was written, an agent on the strongest model was
refused on one account for half a day while a second account stood at 5%.

Changing the secret by hand worked, and showed what a change needs beyond
the secret: the proxy reads it when it starts; the usage readings in the
dispatcher's log describe the account they were taken on, and the standing
shown and the admission rule went on using them; and nothing recorded
which account was in place, so a redeploy or a recovery could not tell.

## Decision

- **Accounts are the installation's.** Its definition lists them in a file
  `model-accounts`: a name, then the command that prints that account's
  credential on the machine that deploys. The credentials stay on that
  machine (a keychain, a password manager); the repository and the cluster
  know accounts by name only. Nothing here says what an account is: the
  names are the installation's.
- **The proxy holds one.** `scripts/deploy.sh CONTEXT model-account NAME`
  pipes that account's credential into the proxy's secret and checks by
  comparing digests that the secret holds it. With no
  name it says which account the proxy holds, by the same comparison, so
  the answer is what is there and not what was last recorded.
- **The choice is in the definition.** The command writes the name to
  `model-account` there, to be committed. A deploy compares it with what
  the proxy holds and says when they differ. It does not switch: an
  account changes when a person or a session on the owner's machine
  decides it, never as a side effect.
- **A change is an event.** The command records `model.account` in the
  dispatcher's log with the account's name. Usage readings from before the
  latest such event are of another account: the standing (`dispatch
  status`) and the admission rule use only those since. The command then
  takes a reading of every model, so the new account's standing is known
  at once.
- **The meter takes it without a restart.** The secret is mounted in the
  meter's pod as a directory, which the cluster changes some tens of
  seconds after the secret by writing a new directory beside it and
  swapping the link `..data` to it. The meter looks at that link and the
  file every two seconds and reads the file again when either changes;
  it takes the new value only if the file did not change while being
  read and is not empty, and otherwise keeps the one in use. A request
  takes the credential once, when it arrives, so one in flight finishes
  on the one it started with. The meter logs `the model's credential
  changed` with a count of changes since it started, never the
  credential nor a digest of it.
- **The command waits for the meter** before it records the change and
  takes the reading, since the reading goes through the meter: until the
  meter's log shows one more change than before, or a new pod of the
  proxy runs (which reads the secret as it starts), for three minutes. If
  the meter does not take it in time, the command says so, records
  nothing, and names `--restart`, which restarts the proxy instead. That
  restart cuts answers in flight, so it refuses while an agent's job runs
  unless told (`--now`). Nothing else is cut, so the switch no longer
  refuses while a job runs.
- **It needs little.** kubectl, the checkout, the definition. The record
  in the log is best-effort, so the switch works with the dispatcher down;
  `deploy/RECOVERY.md` has the same steps by hand. A session on the
  owner's machine other than the one that builds the platform can do it.

## What this does not do

- It does not switch by itself when an account is refused. The dispatcher
  knows when a model is refused (ADR 0052) and could ask for a switch; the
  credentials are on the owner's machine by intent (the owner: a
  credential is kept only where it is needed), so a switch is an action
  there. An agent that is refused can ask a session on the owner's machine
  by message.
- It does not split agents between accounts: one proxy, one credential.
  Per-agent accounts would need the meter to choose by caller.
- The proxy's own credentials (`egress.credentialsSecret`, for hosts
  other than the model's) are read by Envoy when it starts, and a change
  of them still restarts the proxy.
- The meter's signal is its log, read by the command through kubectl: the
  meter answers only inside its pod, and the log is reachable by whoever
  can deploy and by nobody else.
- What each account's use cost is not kept apart: the proxy's record
  (`model.usage`) names agents and models, not accounts. The `model.account`
  events say when each was in place, which is enough to split the record
  by time.

## Consequences

- The installation's definition gains two small files, and the backup and
  the copy in the cluster carry them, with no credential in either.
- Whoever can deploy can choose the account, as they could always replace
  the secret.
- A usage reading can be asked for all models at once (`USAGE_ALL`), which
  the switch uses and a person can too.
