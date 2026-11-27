# new-a-view-holds-only-its-window: A view is a session that holds its one window, not a session of the pod's group

Status: Proposed (2026-11-27). Changes one part of ADR 0057: how a view is
made and how it ends when its window closes.

## Context

ADR 0057 made a view (`agent-view`) a tmux session in the group of the
pod's session `main` (`new-session -t main`), with a hook on
`session-window-changed` that ended the view when tmux moved it off its
window, so that it would not go on to show a neighbouring session.

The check of it in `scripts/test.sh` failed now and then, more often on a
busy machine, and a look once a second was added beside the hook. The
cause, read from the tmux server's log of a run that failed and from
tmux's source:

- When a window of a group closes, tmux rebuilds the list of windows of
  every session of the group. The notice that a session's current window
  changed was recorded with the session's old list, which is gone by the
  time the notice is acted on.
- tmux then acts on it in the place it finds with nothing to go on: the
  session of the client that attached last. It runs that session's hook,
  about that session's window.
- So a view ended by its hook only if it was the view attached last. Any
  other stayed, showing the neighbour's window and passing keys to it.

The check starts two views three milliseconds apart and closes the second
one's window; which of the two reached the server last is decided by the
scheduler, which is why load changed the count. Of 200 runs of that part,
28 failed in one sitting and 3 in another. With the look each second a
view always ended, up to a second late, and until then showed the
neighbour's session.

## Decision

**A view is a session of its own whose only window is the session's
window, linked from `main`** (`link-window`). tmux ends a session when its
last window closes, in the same step that closes the window, whichever
client attached last. A view has no other window to be moved to, so it
never shows a neighbour. There is no hook and no look.

A session cannot be made with no window. The view starts with a window of
its own that is removed in the same command that links the session's,
before anything is drawn; were the linking to fail (the session's window
closed in that instant), that window ends by itself after five seconds
and the view with it.

`main` is untouched as before: its windows, its current window and its
options are as they were, and a window stays in it when a view of it goes.

The check waits for what the views and the pod's tmux say (which sessions
there are, attached, each with how many windows, showing which), reads
that the view is gone straight after the window is closed, with no wait,
and closes windows both ways round: the one of the view made last and the
one of the view made first.

## Consequences

- A view ends in the instant its window closes. The owner is never shown,
  and never types into, a session other than the one gone to.
- The window is listed twice by a command that lists every session's
  windows (`list-windows -a`); nothing in the image does that.
- The check no longer depends on which of two processes started first.
  Its last part (a view whose far end went silent is ended after a time)
  is about a time and still waits in seconds.
