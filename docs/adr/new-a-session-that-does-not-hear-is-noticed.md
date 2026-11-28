# new-a-session-that-does-not-hear-is-noticed: A client refused again and again is recorded once, a pod's watcher gives a session with no listener a turn, and a sender is told of a message that has waited long

Status: Proposed (2026-11-28). Adds to ADR 0053 (the pod's watcher) and
ADR 0045 (what a sender is told of a message).

## Context

An idle session hears a message through its listener (`dispatcher hook
claude-code --wait`, started by the harness at the end of a turn). Three
things were found in use that left a sender with a wrong picture, each of
them silent:

1. The listeners and the watcher of standing pods held a token read once
   at their start. When it expired the dispatcher refused them, several
   times a second, for nine hours. The only trace was warnings in the
   dispatcher's own log; `peers` showed the sessions as "idle, not
   listening", which is also what a session between turns shows. The
   clients now read the token on each request; the dispatcher still has
   no way to say that a client is refused.
2. A listener ended by a signal is not started again by anything. The
   session is open and idle, a message for it is "delivered at its next
   turn", and no turn comes: nobody is at the window.
3. A message can wait for any of a dozen reasons. Each is recorded on the
   message (`message.waiting`), and the sender reads it only by asking
   (`dispatcher message ID`). A sender that does not ask learns nothing.

What the harness does was tried first (2.1.326, with the model's stand-in,
in a tmux server of the trial's own):

- A listener ended with a signal by its process id is not started again
  by the harness while the session is idle.
- A line typed into the idle window (`tmux send-keys -l`, then Enter)
  starts a turn. Its `UserPromptSubmit` hook takes the waiting messages
  into the turn, and the turn's end starts a new listener.
- A line typed while another line is half typed in the box is joined to
  it and both are sent as one: the person's words and the watcher's go to
  the model together.

## Decision

**A client refused again and again is one event.** The dispatcher refuses
a caller whose token it cannot accept. When the token is one the cluster
signed for this dispatcher and this namespace, names a pod, and has only
expired, the dispatcher knows whose client it is refusing. It counts
those by pod, in memory. Five within a minute are a refusal that goes on:
`client.refused` is recorded once (pod, agent, why, since when), and the
owner is sent one message, from the dispatcher, with the pod's sessions
and how it is put right. A refusal ends (`client.refused.ended`) when
nothing of that pod has been refused for two minutes; a dispatcher that
starts ends the ones an earlier one left open. Another client of the same
pod being accepted does not end it: a pod has several clients (each
hook's, the listeners', the watcher's), and a stale one is refused beside
fresh ones.

While it goes on it is shown where a sender looks:

- `dispatch status` lists the pods whose clients are refused, first;
- `peers` says of a session of that pod that is not listening that a
  client of its pod is being refused;
- a message for such a session records it as its reason for waiting
  (`message.waiting`, key `refused`), so `dispatcher message ID` says it.

Only an expired token is counted. A token signed by another issuer, for
another audience or another namespace is somebody else's, and a count of
them would be a count of whatever knocks at the door.

**A session with no listener that a message waits for is given a turn by
its pod.** `GET /v1/attend` also returns the sessions of the pod that are
idle, have no listener, and have a message twenty seconds old or more
waiting. For each, the watcher runs `agent-wake PID 'LINE'` with the
harness process the session's `SessionStart` hook recorded. `agent-wake`
finds the tmux pane that process runs in and types one line there, which
says that a message waits and that the line was typed by the pod's
watcher; the turn it starts takes the message and ends with a listener.

Because a line typed over a person's is sent with theirs, it is typed only
where that cannot happen:

- no tmux client is looking at the window (exit 4 otherwise: a person is
  there, or may come back to a line they left). The first time, the
  client is shown on its status line that a message waits and comes with
  the next line typed;
- the window is not in tmux's copy mode, and the line the cursor is on is
  the harness's prompt with nothing after it (exit 5 otherwise: a line
  typed and left, a menu, a prompt of a harness this does not know);
- the process runs in a pane of the pod's tmux (exit 3 otherwise).

There is a moment between the look and the keys. A person who attaches
and types within those milliseconds gets the watcher's line with theirs;
the line says what it is, so the turn is not misled. This is accepted.

When it does not type, the watcher says why on the message
(`/v1/attend/hold`: someone is at the window, the window is not at an
empty prompt, the session is in no window of the pod's tmux, or it has no
record of its harness), and looks again at each pass, so a window a
person leaves is typed into within ten seconds. When it has typed and the
message still waits after two minutes, it types again after four, eight
and sixteen minutes and then stops, as it does for a session it opens
(ADR 0053). The sender is told each time.

Candidates not chosen:

- *Start the listener from outside the harness.* The listener's end is
  what wakes the harness (the harness started it and reads its exit); one
  started by the watcher would take the message and hand it to nobody.
- *End the harness and open the session again.* It is the same as what
  ADR 0053 does for a dead one and it works, but it throws away a live
  process, and with it whatever a person had in the window.
- *Do nothing in the pod, tell the sender.* That is what the owner's
  machine gets, below. In a pod nobody may look for hours.

**A message that has waited is told to its sender, once.** A message not
delivered ten minutes after it was sent (`dispatcher.messages.waitNotice`,
`off` to turn it off) is recorded as `message.waited`, once for the
message, and its sender is sent a message from the dispatcher: which
message, to whom, how long, and the latest reason recorded for it. This
is for any reason: no listener and nobody to wake it, a pod that will not
open the session, a task that has not started, a limit. The sender is
told only if it can hear: the owner is not sent a message about his own
message to himself, a message from the dispatcher is not followed by
another, and a session that is neither running nor of a standing agent
has the event recorded and no message started for it (it would start a
run to say that something waits). A message to the owner is not counted:
the owner reads when he reads.

**The owner's machine.** No watcher runs there (ADR 0053, not built), and
its terminals are not the dispatcher's to type into. A session there that
is idle with no listener gets the message at its next turn, the reason
says that nothing starts one, and the sender is told after the wait above.

## Consequences

- A refused client is known in about a minute, by an event, by a message
  to the owner and in the three places a sender looks, instead of in the
  dispatcher's log only.
- When the client refused is the watcher itself, nothing in that pod is
  woken or opened: the refusal's message says so, and the senders are
  told after the wait.
- The count of refusals is in memory. A dispatcher restarted while a
  client is refused closes the open event and opens a new one within a
  minute; the owner is told twice.
- An idle session whose listener ended takes a message within about
  thirty seconds when nobody is at its window. Its conversation gains one
  line it did not write and the model is called for one turn.
- `agent-wake` knows the harness's empty prompt by its mark. A harness
  that draws another is never typed into (exit 5), and the sender is told
  that; the mark is in one place.
- A sender gets one more message for each of its messages that waits ten
  minutes. A session that sends many to sessions that are not there is
  told of each.

## The checks, and what stands in for an expired token

`scripts/check-unheard.sh CONTEXT` tries it in a trial with a real harness
and no model: a listener ended by its process id, the line typed and the
message delivered; a client at the window, nothing typed, the sender told
after the wait, and the line typed when the client leaves; a line left
half typed.

A projected token lives ten minutes at the least, so a real expiry makes a
check of twelve minutes or more. It is in the same script behind
`CHECK_EXPIRY=1` and is not part of the ordinary run: the parts that a
real expiry would add to are small (the verifier telling an expired token
of this cluster from any other bad one), and they are tested with a
signed token and a clock (`internal/dispatch/kube`), as is everything
after the door (`internal/dispatch/pgxdispatch`). The long check is for a
change to the verifier or to how pods get their tokens.
