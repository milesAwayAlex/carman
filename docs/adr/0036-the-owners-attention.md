# 0036: An agent asks for the owner's attention with one command, and the owner can pick its conversation up

Status: Accepted (2026-11-22), to be judged by how it holds up in use.
Built; running on the local cluster.

## Context

Agents leave their results where the work is: a pull request, a comment on
a ticket, a document. The owner then has to go looking. They asked for one
place to be told (DESIGN Q, "message delivery"), and chose it when a way to
send appeared: a direct message in the team's chat. Their rule: whatever
the output is, if the agent wants the owner's attention on it, it sends a
message that explains briefly, says how urgent and how important it is,
and gives the link.

The first test also showed what a notice is missing. It is one-way: the
owner cannot answer it. The owner asked that it at least carry something
they can use to reach the agent on the cluster, for an agent still running
and for one whose run is over.

And it showed the danger of letting an agent choose a recipient: the first
test message went to a colleague, because the address was taken from a
pattern that matched two people.

## Decision

- **One command, `notify-owner`, provided by the installation** (among its
  tools; the chat system is its business). It takes a title, a few
  sentences, an urgency (now, today, whenever), an importance (high,
  normal, low) and a link. **The recipient is fixed inside it.** An agent
  cannot use it to message anyone else.
- **Every routine run with all tools is told of it in the same words**
  (`deploy/agent/attention.md`, added to the prompt when the command
  exists): tell the owner once, at the end, if something needs them;
  otherwise send nothing. A routine may say more, for example that it
  always reports.
- **The notice says who sent it and how to reach them.** For an interactive
  agent, how to attach. For a job, its run's name and the id of its
  conversation.
- **A job's conversation outlives its pod.** With `agent.sessionStorage`,
  jobs write their transcripts to a volume that an interactive agent's pod
  also mounts. There, `agent-resume ID` opens that conversation in the
  harness, with everything the job saw and did, and the owner carries on
  from where it stopped. `agent-resume` alone lists the latest.
- **A reply in the chat does not reach the agent.** Not built. The owner
  answers by picking the conversation up, or by attaching.

## Consequences

- A notice is only as good as the agent's judgement of urgency and
  importance. There is no limit on how many an agent sends but the wording.
- The kept conversations hold whatever the job read: they are as sensitive
  as the systems the agent can reach, and they sit on the cluster's disk
  with nothing removing old ones yet.
- The volume is mounted by several pods at once, which one node allows and
  a larger cluster may not; there the conversations need another home.
- Picking a conversation up continues it under the interactive agent's
  role and tools, not the job's.
- An agent that is killed before it finishes sends nothing. The run's
  failure is in the cluster and nowhere the owner looks; that needs
  something outside the agent.
