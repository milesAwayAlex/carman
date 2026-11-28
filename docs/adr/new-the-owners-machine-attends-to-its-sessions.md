# new-the-owners-machine-attends-to-its-sessions: The watcher on the owner's machine says when a session's harness has gone, and shows a notice for a message that waits; it opens nothing

Status: Proposed (2026-11-28). Builds the section "The owner's machine
(not built)" of ADR 0053, for the place of ADR 0049.

## Context

A session on the owner's own machine is believed to run while its
listener is connected or a hook of it was heard within a day (ADR 0049):
no pod says otherwise. So a harness that is killed, or whose terminal is
closed without `SessionEnd`, is shown as running for up to a day, and a
message to it is "delivered at its next turn" when no turn will come. On
a machine with no `/proc` the listener itself could not tell that its
harness had gone (it found none to watch), so it went on listening with
nobody behind it and took the next message.

A message for a session that is not running there waits for the next
session of its name. Nothing starts one: the place is not an agent that a
message starts, and a session there is a terminal a person looks at.

ADR 0053 gave a standing agent's pod a watcher for both. This is the same
watcher for the machine, with what differs said plainly.

## Decision

**The same program, with the place's token.** `dispatcher attend`, run
with `DISPATCHER_PLACE` set and the host role's token (the environment the
hooks have), attends to the sessions of the place. It runs in the
foreground, writes its log to standard error, waits for a dispatcher that
does not answer, and does not end by itself. What keeps it running (a
user service of the machine's) is the machine's configuration, not this
repository's.

**A harness that has gone is said to have ended.** The `SessionStart`
hook writes the harness's process id and start time on the machine too,
under `$XDG_STATE_HOME/dispatcher/harness`, or `~/.local/state/dispatcher/
harness` (`DISPATCHER_HARNESS_DIR` names another place; `/tmp` is not
used: it is others' to read on a machine and cleared under a running
session). The watcher judges it as in a pod: gone at two looks in a row,
then `POST /v1/attend/gone`, which records `session.ended` ("its harness
has gone from the owner's machine") and routes what waits for it as for a
session that is not running; a message a wake handed it with no word
since is delivered once more (ADR 0053). A session with no record gets no
verdict and keeps the day's rule. Records of processes gone thirty days
are removed when the watcher starts.

**Without `/proc`, `ps`.** Where there is no `/proc`, a process's parent,
start time and command are asked of `ps` (`-o ppid= -o lstart= -o
command=`, `/bin/ps` where it is), the start time kept as `ps` writes it.
The watcher asks once per session every ten seconds. The listener, which
looks every two seconds, asks the system only whether a process of that
id is there, and `ps` every thirty seconds whether it is the same one.
With this the listener on such a machine leaves with its harness, as it
does in a pod.

**Nothing is opened, and nothing is typed.** In a pod the watcher opens a
remembered session in a tmux window that is the pod's own. On the machine
the terminals are the person's: several applications, several windows,
none the watcher owns, and a session opened where nobody looks would take
a message and act on it unseen with the owner's own rights. So for a name
that messages wait for and no session runs under, the watcher does one
thing: if the machine names a command (`DISPATCHER_ATTEND_NOTIFY`, or
`--notify`), it runs it once for the messages that wait, as

    CMD NAME COUNT 'FROM: first line of the first message'

with `DISPATCHER_NOTIFY_CWD` (where the last session of that name ran) and
`DISPATCHER_NOTIFY_SESSION` in its environment, and records on the
messages that a notice was shown and when. It runs it again only when
another message comes for the name. What the command does (a notification
of the desktop's, for one) is the machine's. With no command the watcher
does nothing more: the message already says that it waits for the next
session of its name, and its sender is told after the wait that ADR
new-a-session-that-does-not-hear-is-noticed sets.

The stop at the plan's limit that the pod's watcher reads from transcripts
(ADR 0067) is not read on the machine: the person is at the terminal that
shows it.

**Rights.** `GET /v1/attend`, `POST /v1/attend/gone` and `POST
/v1/attend/hold` are opened to the host role, for the place's own
sessions only: the place is told of no pod's sessions and may say nothing
of them, and a pod nothing of the place's. The owner's token still
attends to nothing.

## Consequences

- A session on the machine whose harness died is shown as not running
  within about twenty seconds where the watcher runs, instead of a day.
- A listener left by a killed harness stops within two seconds on a
  machine with no `/proc`; before, it took the next message.
- A message for a session that is not there reaches a person's eye if the
  machine set a command for it; it is still only delivered when a person
  starts or resumes a session of that name.
- Where the watcher does not run nothing changes, except that records are
  written and the listener leaves with its harness.
- One `ps` per session every ten seconds, and one per idle session every
  thirty.

## Not done

Opening a session again on the machine; more than one machine (one place
per installation); reading a limit stop there. The machine's service that
keeps the watcher running, and its notice command, are in the machine's
configuration.
