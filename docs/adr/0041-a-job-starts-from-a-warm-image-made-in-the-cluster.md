# 0041: An agent's job starts from a warm image, made in the cluster and kept in the installation's registry

Status: Accepted (2026-11-25; proposed 2026-11-24). The owner: "i scanned them, looks okay, let's test the system in practice". Built in the chart, the agent image and the
deploy script, and measured on the local cluster. There the warm image was
made by the job's own script run on the node's network, because no pod can
yet reach the node's registry (see Consequences). Not tried on a cloud
cluster.

## Context

An agent's pod took 33 to 50 seconds before the harness could start. The
container itself is up in about a second. Timed step by step on
2026-11-24, the whole of the rest is the harness's installer, which runs
in every job because a job's home is new each time (ADR 0033):

| Definition | Total | The installer | Other |
|---|---|---|---|
| the dreamer's | 44.3 s | 44.1 s | |
| price-review's | 50.7 s | 49.9 s | the cloud identity, 0.5 s |
| one alone, not two at once | about 33 s | | |

This matters more as the system moves to a pod per session (ADR 0040):
every resumed conversation pays it too, with a person waiting.

The owner: "can we build the image from within carman, and only store it
locally/privately using the available storage? we rebuild it two times a
day (let's measure the optimal rebild frequency by the way) and store
privately, so we can start the new sessions. [...] let's also consider how
we could do the same in the cloud, using a private artifact repository."

What the installer does (`https://claude.ai/install.sh`): it reads a
version from `downloads.claude.ai/claude-code-releases/latest`, downloads
that binary and checks its sum, then runs it as `claude install CHANNEL`.
That reads the channel's version from the file of the same name
(`.../stable`), and leaves a 365 MB binary at
`~/.local/share/claude/versions/VERSION`, a link to it at
`~/.local/bin/claude`, and three keys in `~/.claude.json` saying how it
was installed (`installMethod`, `autoUpdates`,
`autoUpdatesProtectedForNative`).

What constrains the answer:

- The harness is its vendor's, and not this project's to hand out (ADR
  0033). An image that carries it stays where it was built.
- Every agent pod runs under the restricted pod security standard, with no
  privilege, and leaves the cluster only through the egress proxy (ADRs
  0031, 0033).
- An agent's home is a volume mounted over whatever the image has there.
- The standing interactive agent keeps its harness in its own home, which
  updates itself (ADR 0034).

## Decision

- **A warm image: the agent image with the harness added as one layer.**
  The layer is `/usr/local/lib/agent-harness/` (`claude`, `version`,
  `channel`), outside the home. The image is labelled `harness.version`,
  `harness.channel` and `harness.base` (the agent image's digest), which is
  how it is known again. It is kept in the installation's private registry,
  beside the agent image, and goes nowhere else.
- **It is made in the cluster, by a job of the installation's own**
  (`agent.warm` in the values, the CronJob `agent-warm`). The job runs
  `warm-harness` in the agent image, under the same pod security as every
  agent pod and through the same proxy. It runs what a pod's start runs, the
  vendor's installer from the channel the installation names, and adds what
  the installer left as a layer.
- **With crane** (go-containerregistry's command line), `crane append`.
  Adding a layer to an image already in a registry needs no builder: crane
  uploads the layer and a new manifest, and the base's layers stay where
  they are in the registry, not downloaded. It is one static binary, runs
  as any user, and also does the rest the job needs: read a digest and a
  config, move a tag, delete. Not chosen:
  - BuildKit, as a daemon of the job's own or rootless: a daemon, and
    rootless BuildKit needs an unconfined seccomp and AppArmor profile.
  - The node's build daemon for agents: a host path and a node group, which
    the restricted standard does not allow, and a feature of one machine.
  - Kaniko, which runs as root and unpacks the base into its own
    filesystem; its upstream has been archived.
  - Buildah, which needs user namespaces or privilege.
  - skopeo and oras, which copy and push but do not add a layer to an
    image.
  - go-containerregistry as a library inside `cmem`. Possible later; the
    job is mostly the vendor's installer and a few calls, which a script
    says plainly.
- **At a pod's start, the copy is linked where the installer would have
  put it** (`agent-env`): `~/.local/share/claude/versions/VERSION` to the
  image's file and `~/.local/bin/claude` to that. Then the three install
  keys go in `~/.claude.json` if absent. If `claude --version` does not
  answer with the image's version, the links are removed and the installer
  runs as before. The harness then counts as a native install, updating
  itself on its channel (its own `claude doctor` says so). An interactive
  agent skips this: its home keeps the installer's copy, and its pod stays
  on `agent.image`.
- **Pods get the newest warm image without a deploy.** Agents' jobs, the
  dreamer, and the pods of resumed conversations (made from an agent's
  CronJob) name `REPOSITORY:TAG` (`warm` by default) with
  `imagePullPolicy: Always`, and the job moves the tag. With `Always` the
  node asks the registry for the tag's digest at each start, a small
  request, and pulls only a layer it does not have. The deploy script
  points the tag at the image it has just built, when that image changed,
  so that the first pods after a deploy start from the new scripts (and
  install the harness). Then it runs the warm job once at its end.
- **Rebuilt only when something changed.** Each run reads the channel's
  version from the file the installer reads (a few bytes), and the agent
  image's digest. It compares both with the labels on the image the tag
  holds, and rebuilds only on a difference. If the agent image changed,
  the tag is first pointed at the agent image itself, so that no pod
  starts with scripts older than the deploy's.
- **A check every twelve hours to start with** (`agent.warm.schedule`).
  Each run prints one line, for collecting:

      warm: at=2026-11-25T00:29:57Z channel=stable version=2.1.325 had=2.1.325 rebuilt=no seconds=1

  `scripts/deploy.sh CONTEXT warm log` prints the line of each run the
  cluster keeps (30 runs, for 14 days); a managed cluster's logging keeps
  them longer. Those lines are the data for choosing the frequency.
- **Plugins stay installed at start.** Measured for an agent with one
  plugin: 3.3 s of its 3.9 s start. The marketplace's clone is 3.0 s, the
  install 0.17 s and its update 0.15 s. Bringing a clone that is already
  there up to date took 5.5 to 6.4 s, longer than cloning it. A marketplace
  in the image would therefore either be stale or start slower. Plugins
  are fetched at start so that an agent has each as it is that day, and
  that is what this costs.
- **On a cloud cluster, the registry and its credential are values.**
  `agent.warm.repository` names a repository in the cloud's private
  artifact registry. `agent.warm.credential: gcloud` has the job prove
  itself with its own cloud identity, through gcloud's helper for
  container tools, with no key. The identity is either the installation's
  workload identity federation (`agent.warm.google`, as an agent's
  `google: true`) or one the platform gives the job's account by
  annotation (`agent.warm.serviceAccountAnnotations`). The registry is
  reached through the egress proxy, like any public address. The nodes pull
  with their own identity, which needs read access to the repository.
- **Old images go.** After each build the job deletes builds older than
  the newest `agent.warm.keep` (3), never one that another tag names. A
  registry that refuses deletes keeps them for its own pruning: on the
  local cluster the platform drops tags not pushed for a week. On a cloud
  registry, a cleanup policy on the repository does it (`deploy/README.md`).
  On a node, the kubelet's image collection removes images no pod uses,
  and the deploy script removes the agent images it built that no pod
  names.

## Consequences

- Measured on the local cluster, 2026-11-25, from container start to a
  harness that answers `claude --version`, on the warm image, in pods made
  from the CronJobs as deployed:

  | Pod | Before | After |
  |---|---|---|
  | the dreamer's job | 44.3 s | 0.15 s |
  | price-review's job | 50.7 s | 0.67 s (0.40 s of it the cloud identity) |
  | price-review's conversation resumed (the resume's Job, set-up only) | as its job | 0.61 s |
  | an agent with a plugin | | 3.9 s (3.3 s of it the plugin) |

  The first pod on a new warm image waits about 4 s more for its node to
  pull the layer, 187 MB compressed. After that, a pod is made and its
  container started within a second.
- Never worse than before: a pod on the agent image, without the layer,
  installed the harness in 30.1 s and worked. A pod on an image whose copy
  does not run said so, installed it, and worked (30.8 s). A failed build
  stops no agent; the job fails and says why in its line.
- Run twice in a row, the job rebuilt (35 to 39 s), then found nothing to
  do (1 s). Two builds of the same agent image and version came out the
  same image, byte for byte.
- **The local cluster cannot make it in the cluster yet.** The agents'
  registry on the node listens only on the node's loopback, which no pod
  reaches, so the job fails at its first question to the registry. It needs the platform to have the registry
  answer, too, on an address of the node's that pods reach (on this node
  the pod network's gateway, `10.42.0.1:5050`). The job's network policy
  opens only that address and port to it. Until then the tag holds what
  the deploy script or a person last put there.
- With `Always`, a registry that cannot be reached stops pods from
  starting. It is the registry the agent image already comes from; to go
  back, turn `agent.warm` off and deploy.
- An agent may start up to one check interval behind its channel. When the
  channel moves, the harness's own updater fetches the new version in the
  background, as on a person's machine; the next check rebuilds.
- **How often the stable channel changes.** No public record lists each
  change. The npm registry's `stable` tag follows the same channel (both
  said 2.1.325 on 2026-11-24). Its archived copies, with today's, show
  2.1.308 (published 2026-10-22) on 2026-10-30, 2.1.323 (published
  2026-11-12) on 2026-11-22 and 2.1.325 (published 2026-11-13) on
  2026-11-24. Stable runs 8 to 11 days behind the newest release and
  moves about once a week; the newest moves about five times a week (67
  releases from 2026-09-01 to 2026-11-20). At twelve hours, nearly every
  check finds nothing, at the cost of a pod start and one small request,
  and a rebuild is about 40 s and 187 MB of registry. Checking hourly
  would cost little more; the job's lines will say whether it is worth it.
- The warm image is for the architecture of the node the job runs on. An
  installation whose agents run on nodes of two architectures places the
  job and the agents on one of them.
- An installation deployed by something other than the deploy script (a
  GitOps controller) has to do the script's two steps itself. It makes
  the tag exist before turning `agent.warm` on (`crane copy` of
  `agent.image`). After each change of `agent.image` it points the tag at
  it and runs the job, or pods start from the old warm image's scripts
  until the next check. A sync hook is the likely shape; not built.
- `claude doctor` in a warm pod reports one warning, the npm copy that the
  locally built image carries as a fallback (ADR 0033). It was there
  before.
- This replaces in part ADR 0033's "an agent installs [the harness] when
  it starts": where a warm image is made, a job links the copy in it, and
  installs only when that copy does not run.
