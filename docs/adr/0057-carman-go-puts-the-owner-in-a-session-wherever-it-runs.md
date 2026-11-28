# 0057: `carman go` puts the owner in a session, wherever it runs

Status: Proposed (2026-11-27).

## Context

The owner's sessions run in several places: named harness sessions in
standing agents' pods (ADR 0034), each a window of the pod's tmux session
`main`; jobs' conversations, kept after their pods (ADR 0040); and sessions
on his own machine (ADR 0049). The dispatcher knows all of them (`peers`,
ADR 0045). Getting to one meant knowing which pod, attaching to its tmux
(`agent attach`), and finding the window; from the checkout, `agent resume
ID` goes to a conversation by id. The owner, 2026-11-27: one command,
`carman go`, from any directory and without the checkout, that lists the
sessions with those waiting on him first and puts him in one; and one
window of his own tmux for each remote session, with the pod's tmux out of
his way.

## Decision

**A program, `carman`, with the subcommand `go`.** `cmd/carman`, a Nix
package of its own (`nix/packages/carman.nix`, the flake's `carman`). It
asks the dispatcher for the sessions and runs kubectl to go to one.

- `carman go`: a picker, `fzf` when it is on PATH and there is a terminal,
  a numbered list otherwise; `--list` prints and exits (`--names`: only
  what each is gone to by); `--print [NAME]` picks or resolves and prints
  what the session is gone to by, going nowhere, for a caller that names
  its own window after it.
- `carman go NAME`, `AGENT/NAME`, `session/ID`, or the start of an id (four
  characters or more). A name two agents' sessions have means the one that
  runs when only one does; otherwise it is an error that lists them.
- What a session is gone to by, everywhere: `AGENT/NAME` when a name
  reaches it, else `session/ID`.

**The order: who needs the owner most.** From what the dispatcher knows: a
session that is idle and has sent the owner a message he has not read;
then one that has sent him one and is working; then idle sessions; then
busy ones; then those not running that can be gone to (a standing agent's
session that its name still reaches, whose window the pod opens again, and
jobs' kept conversations). Within each, the one heard from last first. For
this, a peer now carries its pod, how it runs, whether its agent is a
standing one, and how many messages it sent the owner that are unread. The
last is given to the owner and to the owner's machine only: the machine's
token does not read the owner's inbox (ADR 0049), and still does not; it
learns how many wait from each session, which is what the list needs.
Sessions on the owner's own machine are not listed: they are in a terminal
of his already.

**Where it goes.**

- A session in a standing agent's pod: `kubectl exec -it POD -- agent-view
  ID NAME`, in the cluster and namespace of the configuration.
- A job's kept conversation: the checkout's `scripts/deploy.sh CONTEXT
  agent resume ID` when `CARMAN_CHECKOUT` names a checkout, which makes the
  pod that goes on as its agent (ADR 0040); with none it says so, and what
  to run. That logic moves into the program later.
- A headless run that is going on has no terminal: it says so.

**Configuration is the environment.** `CARMAN_CONTEXT` (the kubectl
context; nothing is run without it), `CARMAN_NAMESPACE` (`carman` unless
set), `CARMAN_DISPATCHER_URL` and `CARMAN_DISPATCHER_TOKEN` (or
`DISPATCHER_URL` and `DISPATCHER_TOKEN`, as the client has them: the
owner's machine's token is enough), `CARMAN_KUBECTL` (`kubectl`),
`CARMAN_CHECKOUT` (optional).

**In the pod, each client has a view of its own.** Clients attached to one
tmux session share its current window, so two windows of the owner's tmux
could not show two sessions of one pod. `agent-view` makes, for each
attach, a session in `main`'s group (`new-session -t main`): the same
windows, a current window of its own. It finds the session's window by the
harness process its `SessionStart` hook recorded (ADR 0053), or by name,
after asking `agent-open --reopen` for a session that is not running. In a
view:

- no status line and no prefix key (session options, so `main` keeps its
  own for `agent attach` and the watcher): the owner's tmux is the only one
  he drives, and every key reaches the harness;
- the mouse is on, so that the wheel scrolls back through the session's
  history, which with no prefix has no other way in;
- `destroy-unattached`: the view goes when its client does, however that
  is; a view left with no client is removed by the next one;
- a hook ends the view when its window closes, so that it does not go on
  to show a neighbouring session under the same name.

There is no key to leave a view from inside. It is left by closing the
window of one's own tmux that holds it, or by ending the session (its
harness, then the shell it leaves).

**A dropped connection.** `kubectl exec` ends when the laptop sleeps or
the network changes. How the attach ended says which it was: `agent-view`
ends with status 0 when the view was left (its client went, or its window
closed) and with 3 when there is nothing to show, and kubectl gives its
status back; any other ending is a drop. A signal to the program itself
(its terminal closed, an interrupt while it waits) is a leave. Without
`--keep` a drop is an error. With `--keep` it attaches again after two
seconds, doubling to thirty, asking the dispatcher each time which session
the name reaches by then; five attaches in a row that each fail within five
seconds end it with an error. So `carman go --keep NAME` ends with 0
exactly when the session was left, with 3 when there is nothing to go to,
and with another status on an error; a cancelled picker is 130.

**What a drop leaves in the pod.** The view's client there is not hung up
when the connection goes (seen from the owner's machine, 2026-11-27: the
kubectl killed, the client stayed attached, and its view with it). Two
things end it. The next attach from the same place to the same session
ends the client before it (`AGENT_ATTACH_ID`), as `agent-attach` does. And
for the case that no such attach ever comes (a laptop closed, another
terminal opened later), the far end says it is there: beside each attach
the program keeps a second `kubectl exec` with no terminal (`agent-view
--beat`) and writes it a line every twenty seconds; a view that asked for
this (`AGENT_VIEW_BEAT`) and has heard nothing for ninety seconds is ended
in the pod, with the reader the drop left waiting. When the program itself
leaves (its window was closed), it says so on the same channel, and the
view is ended at once (without that word a closed window's view lasted
the ninety seconds: seen from the owner's machine, 2026-11-27). Its client ends as a
drop does, so a program that is still there and keeps (`--keep`) attaches
again. The beat's process is in a process group of its own, and an
attach that a signal ended just before a signal reached the program is a
leave too: a terminal that goes hangs up its whole foreground group at
once when the program is not the session's leader (run under a shell,
seen from the owner's machine, 2026-11-28: the view stayed until the
silence ended it), and the bye has to outlive that. A view attached by hand, with no beat, lives until its client goes.

**A new session** (added 2026-11-27, when the owner retired the one
long-lived terminal he opened sessions from). `carman go --new NAME [--in
AGENT] [--dir DIR] [--model MODEL] [--keep]` opens a named session in a
standing agent's pod and goes to it.

- Which agent: the one `--in` names; with one standing agent, that one;
  with several and none named, the owner is asked on a terminal, and with
  no terminal it is refused with their names. The program knows them from
  the dispatcher's peers: an agent now says whether it is a standing one
  (`standing`, as its sessions already did), so that one with no session
  yet can be opened in. Its pod is the one its sessions were last seen in,
  or else the one pod of its StatefulSet (`agent-NAME-0`), as the chart
  names it and as `agent attach` has it.
- Where: `--dir` is a directory in the pod. With none it is the pod's
  `~/work`, where the pod's own tmux and its sessions start (`agent-open`
  with no directory means where it is run, and `kubectl exec` runs it
  nowhere in particular); one that is not absolute is under `~/work`. A
  directory that is not there is an error: nothing is made. `--model` is
  `agent-open --model`.
- How: `kubectl exec POD -- sh -c SCRIPT`, which runs `agent-open` there,
  so the session is remembered by the pod and opened again when it starts,
  like any other of its sessions. The name, the directory and the model
  are the script's arguments, never part of its text. It is `agent-open`
  as the pods have it, so a pod need not be restarted for a new image.
- A name that is taken is refused, and nothing is opened: when the
  dispatcher knows a running session of that agent and name, before
  anything is run; and in the pod, when the name is remembered there or
  has a window. It is not gone to instead: whoever asks for a new session
  names a directory and a model, and would be put in another conversation,
  somewhere else, with nothing to tell him so; the refusal says `carman go
  AGENT/NAME`, which is one command more. The check is before `agent-open`
  because `agent-open` with a remembered name gives it the new directory.
- Then it is gone to exactly as `carman go NAME` is: a view of its window
  (ADR 0071), with `--keep` if asked. Its id is not known yet, so the view
  is found by the window's name (`agent-view '' NAME`); after a drop
  `--keep` asks the dispatcher for the name, as it does for any session.
- The list is the dispatcher's, and the dispatcher learns of a session
  from its harness (its `SessionStart` hook). Until then, a second or so
  after it is opened, `carman go --list`, the picker and `carman go NAME`
  from another window do not know the new session; the window that opened
  it shows the harness starting. If the name was used in that pod before,
  the list shows that earlier session, not running, for that moment.
- A name used before, in the same directory: `agent-open` goes on with the
  latest conversation of that name there, as it does at a pod's restart.
  A new conversation is a new name.
- Ending a session for good is as before: `/exit` in it, then `exit` in
  the shell it leaves, which closes its window and makes the pod forget
  it; its conversation is kept. The usage text says so. A form of `carman
  go` for it is not built: forgetting alone (`agent-open --forget`) would
  leave the harness running and listed, and ending a harness from outside
  is `agent-open`'s to do first.

## Tried

In a standing agent's pod, with two trial sessions and a tmux server of
its own standing for the owner's: two views showed the two windows at
once, each sized by its own client (80x20 beside 110x30); shift+tab,
escape, vim's keys and the pod tmux's own prefix all reached the harness;
`main` kept its status line and prefix; closing a session's window ended
its view with status 0; closing the outer window removed the view. Not
tried here: `kubectl exec` from outside (the pod has no rights to exec),
the wheel and the clipboard in a real terminal, and a real drop.

`--new`, in a standing agent's pod, with a kubectl that runs there what
follows `--` and a tmux server of its own standing for the owner's
terminal: a session opened under a new name, in a directory under `~/work`
and on a named model, was remembered by the pod with both, shown in the
view at its prompt, and listed by the dispatcher 0.8 seconds after the
command was started; the same name again was refused, with nothing
changed; `/exit` and then `exit` ended the program with status 0, closed
the window and made the pod forget the name; closing the outer terminal
left the session running and removed its view. Not tried: through a real
`kubectl exec`.

## Consequences

- The owner needs kubectl with rights to exec into agents' pods, which he
  has; the program holds no credential beyond the dispatcher's token.
- What the owner's tmux does with it (a window for each session running
  `carman go --keep NAME`, a key for the picker) is the machine's
  configuration, not this repository's.
- Copying text: selection with the mouse is tmux's in the pod (the view
  has the mouse); it reaches the owner's clipboard as the pod's tmux
  configuration sends it (OSC 52), as in `agent attach` today.
- Notices are unchanged: a link from a notice to a session waits for the
  channel it would be sent through.
