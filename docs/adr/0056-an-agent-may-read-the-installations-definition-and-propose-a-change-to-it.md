# 0056: An agent may be given the installation's definition to read, and proposes a change to it as a patch the owner's side applies

Status: Proposed (2026-11-27).

## Context

An installation's definition (its values, its agents' routines, its
commands) is a git repository on the owner's machine, which the deploy
script reads (`deploy/README.md`). Each deploy puts a bundle of it in the
cluster, as the config map `installation`, for the nightly backup.

The session that builds the platform now runs in a standing agent's pod.
It builds, tests and tries changes there (ADR 0054), and a session on the
owner's machine merges and deploys them. One thing it could not do: when a
change needs a value of the installation switched on (an agent added, a
flag set), it said so in words, and the session on the owner's machine
typed the commit. The definition was neither readable from the pod nor
changeable by anything but hand.

## Decision

**Reading: an agent the installation chooses.** `definition: true` on an
agent's entry mounts the config map `installation` read-only in its pods,
at `/etc/installation/definition.bundle`, as the backup's job has it.
`agent-definition clone` makes a working clone of it
(`~/work/installation`). Not every agent: the value grants what the agent
could not see before, which is

- every agent's entry, not only its own: schedules, models, permission
  modes, plugins, MCP servers, the names of variables, node paths, groups;
- the hosts the egress proxy holds a credential for and the name of each
  credential's key (not its value), the cloud identities by name, the
  namespaces agents reach;
- the accounts for the model by name, with the command that prints each
  one's credential on the owner's machine (ADR 0055): a command line, which
  may name an item of a password manager;
- the history of all of it: every earlier version, who committed it and
  why, on every branch and tag.

The routines and commands themselves every agent's pod already has.

**It holds no credential, as far as the script can tell.** A bundle holds
only what is committed. Checked against the script, with two changes made
here: the bundle was made with `--all`, which also takes a stash (and a
stash holds what was not committed, untracked files too if it was made so);
it is now made of branches and tags. And the files this script itself
writes beside the definition that hold a credential or are one machine's
own (`cmem.env`, `cmem-dreamer.env`, `dispatcher.env`,
`dispatcher-owner.token`, `bench.env`) are looked for in the history of
every branch: if one was ever committed, no bundle is put in the cluster,
and the deploy says so. That a credential was committed under another name
the script cannot know: not committing one is the repository's rule
(`deploy/README.md`), and `definition: true` relies on it.

**Proposing: commits, sent as the patch git makes of them.** The agent
commits in its clone. `agent-definition propose PLACE/NAME` sends the
commits that are not deployed to a session on the owner's machine, in one
message: a first line that says what the change is ("A proposed change to
the installation's definition: SUBJECT"), which files it touches, how to
apply it, and between two marker lines the output of `git format-patch`
(full object ids, binary changes included), with the commit it was made
against. The commits are authored as the agent ("agent NAME", at an
address that is nobody's), whatever identity the pod's git has for commits
elsewhere: `propose` makes them so. A message holds 64 kB; a proposal over 60 kB is refused at the
agent, which makes it in smaller commits, each applied and deployed before
the next. A clone that is behind what is deployed is brought up to date
first: a proposal is against the definition as it is.

**Applying: an action on the owner's machine, that deploys nothing.**
`scripts/deploy.sh CONTEXT definition apply MESSAGE-ID` reads the message
through the owner's client (or `--file FILE`, its text saved), and applies
the commits to the definition with their authors and messages, each with a
trailer `Proposed-in: message ID, from SENDER`. The author says which agent
by its own word; who sent it is the dispatcher's record. The commits are
tried first in a checkout beside the definition, and what they touch is
read from git's trees, not from the patch's text. The whole proposal is
refused, and nothing changes, when

- it does not apply as it is (the definition has moved on where it
  changes): the sender updates its clone and proposes again;
- the definition has changes that are not committed;
- a commit touches git's own files (`.gitignore`, `.gitattributes`,
  `.gitmodules`), a file the definition's rules ignore, one of the
  machine's own files above, a symbolic link or a repository within it;
- a commit adds a file anywhere but under `routines/` or `tools/`. What
  the definition tracks may change; a new file elsewhere (one the deploy
  script would read or source, such as `bench.env`) is for the owner's
  side to add.

It then prints the commits, the files, and what to look at closely, and
how to take it back (`git reset --hard` to the commit before). Deploying is
a separate step by whoever read it.

**What the one who applies is trusted to check.** A proposal is a request
from an agent, with the standing a message from an agent has: the session
on the owner's machine that receives it, or the owner, decides. The checks
above keep a patch inside the definition; they do not judge it. What a
change to the definition can do that needs reading:

- `values.yaml`: anything an agent may reach or do. A permission mode, a
  node path or group, a host the proxy adds a credential for, a cloud
  identity, who may write to an agent, the sandbox's reach, an image, where
  the model's credential is held, `definition: true` itself;
- `tools/`: a command that agents run, with whatever those agents reach;
- `routines/`: what an agent is told to do, in words;
- `model-accounts`: a command the deploy script runs on the owner's
  machine when the account is switched. A change to it is code that runs
  there.

The action names the last three when a proposal touches them.

**Staying in step.** A deploy replaces the config map, and the pod's mount
follows it (seen: within 40 to 80 seconds). `agent-definition update`
fetches from the bundle and puts the clone's own commits on top of what is
deployed; a commit that has been applied and deployed drops out, since git
knows a commit that makes the same change. If the agent's commits no
longer fit, the clone is left as it was and the agent makes the change
again. A proposal already sent against an older definition is applied if
it still applies, and refused whole if not.

## Consequences

- A change to the installation proposed from a pod is the same commit on
  the owner's machine, with nothing typed twice.
- An agent given the definition sees what the installation says about
  every other agent, and the names of its accounts and hosts.
- The first proposal needs the definition, and so one commit by hand:
  `definition: true` on the agent's entry.
- A trial installation (ADR 0054) can stand for all of it:
  `TRIAL_DEFINITION=DIR scripts/trial.sh CONTEXT up` puts a definition's
  bundle in the trial and runs a pod that has it.
- Any session may send a message that holds a patch, whether or not its
  agent was given the definition. Nothing is applied by receiving one.
