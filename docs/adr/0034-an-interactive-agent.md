# 0034: An agent can be interactive: a standing pod a person attaches to

Status: Proposed (2026-11-20). Built and running on the local cluster.

## Context

Every agent so far is a job: a routine, run to the end, reported in a log.
The owner wants to work with an agent in the cluster the way they work
with one on the laptop: start it, talk to it, leave, come back. They asked
whether the persistent disk and the shared memory could be what suspends
and resumes a conversation, with tmux or the like to connect to.

The owner's decisions, 2026-11-20: such a session has the same access as
every other agent, by keys held at the egress proxy, and no login of its
own; it works in the laptop's role in memory, since a person is at the
keyboard; the harness runs in its automatic permission mode; which
repositories it works on is the agent's to decide from the conversation;
and the shell should be the owner's own, not a bare one.

What was found by trying:

- The harness starts interactively with the same model credential the jobs
  use. It asks for no login.
- Its automatic permission mode is available with that credential.
- It remembers that a directory is trusted, except for the home directory
  itself; started there, it asks at every start.
- A machine-wide settings file is not read when the account's organisation
  sends settings of its own. Hooks put there never ran.
- A login shell, which is what a tmux window starts, resets PATH.

## Decision

- **An entry under `agents` with `interactive: true` is a standing pod**, a
  StatefulSet of one, in place of a CronJob. It has everything a job can
  have (tools, plugins, MCP servers, the cloud identity, the proxy as its
  only way out) from the same pod definition.
- **Its home is a volume that outlives the pod.** The harness keeps its
  transcripts there, so `claude --continue` picks a conversation up after
  the pod or the machine has restarted. Checkouts, shell history and
  anything else in the home last the same way.
- **The session is tmux inside the pod.** `scripts/deploy.sh CONTEXT agent
  attach NAME` attaches a terminal to it; detaching leaves it running. No
  port is opened: the way in is the cluster's own exec.
- **Work happens in `~/work`**, which the harness trusts once.
- **Sessions can be named and are opened again.** `agent-open NAME [DIR]`
  starts the harness as a named session in a window of its own and
  remembers it in the home. A restart of the pod ends every process in it;
  at the next start each remembered session is opened again on the latest
  conversation of its directory. What was running at the moment of the
  restart is lost; the conversation is not. One session to a directory.
- **A deploy does not restart it.** It is replaced only on purpose, because
  a person's running sessions are in it; until then it runs the image it
  started with.
- **Memory role `laptop` unless the entry says otherwise**, permission mode
  `auto` likewise.
- **The memory hook is on for every agent that does ordinary work, jobs
  included**: the index when a session starts, a flashback where a memory
  is likely to matter. Jobs ran without it only because the first agent,
  the dreamer, must: it reads the whole library itself. The dreamer and the
  backup still switch it off. The hook goes in the user's settings when
  the agent starts, for the reason found above.
- **One script sets an agent up** (`deploy/agent/agent-env`: trust in the
  proxy, the cloud identity, the harness's settings, plugins), used by jobs,
  by the session, and by login shells.
- **The shell is the installation's.** The agent image can be built on a
  base the installation names (`CARMAN_AGENT_BASE`), which brings its own
  shell, tmux, editor and tools; steps it leaves in
  `/usr/local/lib/agent-start.d` run at each start, which is how
  configuration reaches a home that is a volume. Without one, the image
  has a plain bash and tmux.

## Consequences

- The pod holds the laptop's memory token, so what the agent there writes
  can cite a person. That is right while a person is driving and wrong if
  the session is left to work alone on something; the owner accepted it
  for a session they start themselves.
- An agent with every tool, in automatic mode, on a home that persists:
  what it installs or breaks there stays until someone removes it. The
  limits are the same as a job's and are around the pod, not in it.
- It is reached only from a machine that can reach the cluster's API.
  Reaching it from another machine or a phone is not built.
- Not the same as the laptop: the harness's own connectors and its remote
  control need a login the pod does not have.
- The harness's first-start questions (theme, a notice, trust in `~/work`)
  are asked once and remembered in the home.
- The memory hook names a directory that is not a repository after the
  directory, so in `~/work` itself a session's scope is `repo:work`; inside
  a checkout it is the repository's own.
