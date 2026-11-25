# 0040: A kept conversation is resumed as the agent that had it, in a pod of its own

Status: Accepted (2026-11-25; proposed 2026-11-24). The owner: "i scanned them, looks okay, let's test the system in practice". Built and tried on the local cluster. It
supersedes the part of ADR 0036 that has a job's conversation
picked up from an interactive agent.

## Context

The dreamer ended a nightly report with a question for the owner. The
owner got the notice, opened the conversation with `agent-resume ID` in the
standing interactive agent's pod (ADR 0036), and answered. The agent could
not act on the answer: it was running in the interactive agent's pod, with
that pod's credential, and the memory refused it the dreamer's work. The
owner: "i did reply to it in the interactive session, but it's unclear how
the agent i replied to will act".

ADR 0036 had said as much among its consequences: a conversation picked up
there continues under the interactive agent's role and tools, not the
job's. The cause is that a conversation is resumed wherever the owner
happens to be, not as the agent that had it.

The direction the owner has agreed for the system: a session is the lasting
thing (a conversation, a working directory, and a profile: image,
credentials, tools, memory role), and pods are disposable places where a
session runs for a while. This is the first step of that.

## Decision

- **`scripts/deploy.sh CONTEXT agent resume ID` resumes a kept conversation
  as its agent.** It makes a pod from the agent's own CronJob: the same
  image, environment, credentials, mounts, account and proxy. In it the
  harness is opened on the conversation inside tmux, in the directory the
  conversation was held in, and the command attaches. Detaching leaves the
  pod; the same command attaches to it again, found by its label, instead of
  making a second. Leaving the harness and then its shell ends the pod, and
  the cluster removes it. Without an id, the command lists the kept
  conversations and where each is open.
- **The pod is a Job of its own, not the CronJob's.** It has no deadline,
  so that a person is not cut off, and it does not count as a run of the
  agent, so that a pod left open does not hold up the agent's schedule. It
  carries the conversation and the agent as labels and its start as an
  annotation, which is what an idle limit will need.
- **The agent is found where the conversations are.** A job's conversation
  is kept under a directory named for its agent (ADR 0036). The command
  reads that volume from a pod made for the purpose: read-only, with no
  credential, removed after. It does not use an interactive agent's pod,
  which may not be running and may be on an older image.
- **The pod runs the script of the checkout that starts it**
  (`deploy/agent/agent-resume`), handed in whole, so a resume does not
  wait for an image that carries it.
- **Attaching lands in the conversation.** The pod's home is new each time,
  so before the harness starts it is told what its first start would ask:
  onboarding done, the directory trusted, and for an agent with every tool,
  the warning about running without asking seen. There is no sign-in: the
  model credential is the job's, as in ADR 0034.
- **The model is the one that last answered in the conversation.** A job
  picks a model for each attempt up its ladder (ADR 0029), and each attempt
  is a conversation of its own, so the conversation's model is the one
  whose words the owner is answering. If none can be read, the first of the
  ladder.
- **The agent's rights are its job's, with a person at the keyboard or
  not.** An agent with every tool runs without asking, as its job does; its
  limits are around the pod. An agent with the memory command alone, as the
  dreamer, may run that and nothing else, and what else it tries is refused,
  not put to the person: approving a prompt would give the agent more than
  its role. The owner's answer is an instruction to the agent, which acts
  on it within its own role, and no more than it. The shell left in the
  pod after the harness is the owner's.
- **One writer for a conversation.** Whatever opens one holds a lock on a
  file beside it, `ID.lock`, and writes in that file where it is open: the
  pod and since when. The lock is the kernel's, so it goes with its holder
  however that ends, and every pod on the node sees it. A second opener is
  refused with where the conversation is open. A harness in an interactive
  agent's pod that has the conversation open without the lock (started by
  hand, or by `agent-resume` before this) is found by its process, and the
  command refuses as well.
- **`agent-resume` in an interactive agent stays**, for reading a
  conversation or carrying it on with that pod's rights. It respects the
  lock, and before it opens another agent's conversation it says that the
  conversation will go on with this pod's rights, not that agent's, names
  the command that resumes it as the agent, and asks.
- **The notice a job sends when its report has questions names the command
  that resumes the conversation as the agent.**

## Where this leads

A session is what lasts: a conversation, the directory it works in and a
profile, which today is the agent's definition. A pod is where a session
runs while someone or something needs it. A dispatcher will later start
one where a message for a session arrives and end it when it has been idle
for about an hour, which is when the harness's prompt cache has lapsed and
the harness would compact the conversation anyway. The interactive agent's
named sessions and the jobs' conversations then become the same kind of
thing.

## Consequences

- The owner can answer an agent's question and have the agent act on it:
  the resumed dreamer has the consolidation role and the memory command, as
  in its nightly run. A question whose answer needs more than the agent's
  role is still answered elsewhere.
- A pod nobody is attached to stays, with the agent's credentials, until
  someone removes it. Nothing ends an idle one yet.
- A scheduled run of the agent can run beside a resumed conversation of the
  same agent. For the dreamer, a nightly consolidation and one the owner is
  directing could both write; consolidation is checked as it writes and a
  run is reverted as a unit (ADR 0029). This was preferred to an open pod
  that stops the schedule.
- The conversation's working directory comes back, its contents do not: a
  job's `/work` is empty at the start of a run and gone at its end. A
  resumed session checks out again what it needs.
- A job's spending cap does not apply to a resumed session; the owner is
  there.
- The harness in the pod is the one its channel gives that day, which may
  be newer than the one that held the conversation.
- The lock relies on the conversations' volume being on one node, which
  ADR 0036 already requires.
- What runs in the image changes with the next image: `agent-resume` in an
  interactive agent (the warning and the lock) when that agent is
  restarted, and the notice at the next run. The resume itself does not
  wait for an image.
