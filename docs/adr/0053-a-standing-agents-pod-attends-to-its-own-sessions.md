# 0053: A standing agent's pod attends to its own sessions: it says when a harness has gone, and opens a remembered session a message waits for

Status: Proposed (2026-11-27)

## Context

A standing agent (`interactive: true`, ADR 0034) is a pod that stays, with
named harness sessions in tmux windows (`agent-open NAME DIR`), remembered
and opened again when the pod starts. Messages reach such a session as
`AGENT/NAME` (ADR 0045). ADR 0045 left two gaps, both of which make the
sender's picture wrong:

1. A session's end is known from its `SessionEnd` hook or from its pod
   going. A harness killed (out of memory, a crash, `kill -9`) or whose
   window was closed without the hook running goes on being shown busy or
   idle, and a message to it is "delivered at its next turn" for ever.
2. A message for a named session that is not open waits for ever. A job's
   session that is not running is resumed by a task (ADR 0040); a standing
   agent has no job, and nothing opened its session again.

Only something inside the pod can see the harness's process and open a
tmux window there. The dispatcher sees the pod, not what runs in it.

## Decision

**A watcher in the pod.** `dispatcher attend` runs in every standing
agent's pod, started by `agent-session` beside the remembered sessions,
and started again if it ends. It uses the pod's own token for the
dispatcher, the one the hooks use, and looks every ten seconds.

**How a dead harness is known: by its process.** The `SessionStart` hook,
in a standing agent's pod, writes which process is the session's harness:
the nearest ancestor of the hook whose command is the harness's, by its
process id and its start time (`/proc/PID/stat`), in
`/tmp/agent-harness/SESSION`. The watcher holds a session that the
dispatcher believes runs in this pod to have ended when that process is
not there, or is another process with the same id (a different start
time), at two looks in a row. Then it tells the dispatcher, which records
`session.ended` ("its harness has gone from its pod") and routes what
waits for the session as for one that is not running.

The candidates, judged on false endings and cost:

- *The process* (chosen). A harness busy in a long tool call, or idle
  with no listener, is a live process: no false ending. The only false
  "alive" is a process id reused, which the start time rules out. It
  costs reading two small files per session. A session with no record (a
  harness started before this was deployed, or one whose hook could not
  find its process) gets no verdict and is left as before.
- *The tmux window of that name.* The window outlives its harness by
  design (`agent-open` leaves a shell in it, so that a person keeps their
  place), so a window that is there says nothing; and sessions not opened
  by `agent-open` have no window of their name.
- *No listener and no word for some time.* A session busy in a long tool
  call has no listener and says nothing for as long as the call lasts;
  any threshold either ends live sessions or takes hours to see a dead
  one. The owner's machine uses it (ADR 0049, a day) only because nothing
  better is there.
- *ADR 0040's one-writer lock.* Standing sessions do not take it:
  `agent-open` opens a conversation in the pod's own home, not on the
  conversations volume. Taking it would be the same signal as the
  process, with more to go wrong.

**A message for a named session that is not open waits for the pod to
open it.** For a standing agent (an agent with no job) and a session of
it that has a name, the dispatcher records the message waiting "for the
agent's pod to open it again". The watcher asks the dispatcher which
names of its agent have messages waiting for a session not running, and
for each runs `agent-open --reopen NAME`:

- The name is remembered here (`agent-open`'s list) and has no window:
  its window is made, as at the pod's start.
- Its window is there with the shell its dead harness left at its prompt:
  that shell is replaced by the session (`tmux respawn-window`), on its
  conversation, as `agent-open NAME` does. What runs in the window is
  judged by its processes, not by tmux's name for it (a live harness that
  `agent-open` runs shows as the script's shell): a harness, a shell with
  a job of its own, or a window a person attached is looking at is left
  alone.
- Its window runs anything else: it is left alone (exit 4), and the
  sender is told so; it is tried again later.
- The name is not remembered (forgotten with `agent-open --forget`, left
  with `exit`, or never opened here): it is not opened (exit 3); the
  message waits and its sender is told why.

The session's listener takes the message, which starts a turn (an
interactive session's start is not one: what its `SessionStart` hook took
would sit in its context at an idle prompt): a named session of a standing
agent's pod is handed, as on the owner's machine (ADR 0049), the messages
of the earlier sessions of its name that are not running, since the
harness may give a reopened session a new id. A session of that name
opened by hand takes them the same way.

**Limits.** The watcher opens only a name that a message waits for, one
session for it, once. If the messages still wait after two minutes (the
harness did not start, or did not take them), it opens it again after
four, eight and sixteen minutes, and stops after four opens until another
message comes; each time the sender is told, on the message
(`dispatcher message ID`). What it has tried is kept in a file beside the
records, so a watcher started again does not open at once what it just
opened. A dispatcher that does not answer is waited for; the watcher does
not end. The dispatcher may restart between any two of its calls: each
call is a question or an idempotent statement.

**No new rights.** The pod's token already let it say a session of its
own agent in its own pod had ended (`/v1/sessions`). Three calls are
added, for the `agent` role only, with a pod's own token, and only for
the pod's own agent: `GET /v1/attend` (the sessions of this agent the
dispatcher believes run in this pod, and the names of this agent with
messages waiting), `POST /v1/attend/gone` (a session of this agent whose
last pod is this one has ended, with why), and `POST /v1/attend/hold`
(why a message for a session of this agent still waits, recorded as a
`message.waiting` with a key of the pod's, once per key). Another agent's
pod, another live pod of the same agent, the owner and the owner's machine
are refused.

## Consequences

- A harness that dies in a standing pod is shown as not running within
  about twenty seconds, and a message to it opens it again where the
  person can attach and see it.
- `dispatcher message ID` says why a message for a standing session still
  waits: not remembered, its window busy, or opened and not taken.
- A harness the hook could not find (an unknown launcher whose command is
  neither `claude` nor `node` with a `claude` path) is never judged dead;
  the watcher's log says nothing of it. The harness's own command line is
  what is matched.
- Replacing the shell left in a window ends whatever that shell held only
  if it was at its prompt: a person who ran something in it keeps it.
- The watcher is one more process per standing pod, with one request to
  the dispatcher every ten seconds, which reads the agent's sessions.

## A wake that reached a harness that had gone

The listener stops when its harness goes, but looks only every two
seconds: a message sent in that time is taken by a listener with nobody
behind it, and recorded as delivered (seen in the second trial,
2026-11-27, with one second between the kill and the send). The owner's
session, asked: err on the side of a message seen twice, "a lost message
is silent, a repeated one is visible".

So: when the pod's watcher reports a session's harness gone, a message
that a wake handed that session, with no word from the session since, is
recorded as not delivered (`message.undelivered`, once for a message) and
is routed again as a message for a session that is not open; the pod opens
the session, and its listener takes it. Kept narrow:

- only a delivery by a wake (the idle listener's), which is the one
  delivery whose taker may have nobody behind it; what a hook of a turn
  took was put in a running harness's conversation;
- only when the end is the watcher's word that the process has gone, not
  when the session said itself that it ended (its `SessionEnd` hook ran,
  so the harness was there to have the message);
- only with no word from the session after the wake: any other hook of it
  (a tool call's, the turn's end) or any state it reports means its
  harness had the message. A message the session read or answered is
  left as it is;
- once: a message is delivered at most twice.

**What a session makes of a message it sees twice.** A harness that died
after the message reached its conversation, and before its next hook, has
the message in its transcript; opened again on that conversation, it is
handed the same message, with the same id, and is told that this is a
second time and why. It looks at its own conversation: if it shows that
it acted on the message, it does not act twice, and finishes what was
left or answers that it was done; if it shows nothing, it acts. The
sender sees both deliveries and the reason between them in
`dispatcher message ID`.

## Found in the first trial in a pod (2026-11-27), and changed

- The idle listener (`dispatcher hook claude-code --wait`) outlived a
  harness killed with `kill -9` and took the next message for the session,
  as a wake to a harness that was gone: the message was marked delivered
  and the session busy again. The listener now listens only while the
  harness above it is there (by its process and start time, as the
  watcher judges it), and stops when it goes. What it takes in the two
  seconds before it notices is delivered again (the section above).
- The watcher removed a session's record when it said the session had
  ended, so a session the dispatcher came to believe runs again was never
  judged again. The record is kept.
- A session opened again for a message took it in its `SessionStart` hook
  and sat at its prompt without acting on it. An interactive session's
  start takes nothing now; its listener does, which starts a turn.
- `agent-open --reopen` judged a window by tmux's name for what runs in
  it, which for a live harness started by `agent-open` is the script's
  shell, and replaced a live harness (tried on a trial session). It now
  looks at the window's processes.

## The owner's machine (not built)

The same watcher would run there with the place's token (ADR 0049). What
would differ: the caller is the place, not a pod, so "in this pod" becomes
"at this place", and a machine may have several harnesses in several
terminals that no tmux of the watcher's owns; the record of the harness's
process works the same (the hook already runs there). Opening a session
again needs a terminal a person will look at, which is the machine's own
business (a tmux session of the user's, or a terminal application), and
a list of remembered sessions the machine keeps; until then the place's
rule (a message waits for the next session of its name) stays.
