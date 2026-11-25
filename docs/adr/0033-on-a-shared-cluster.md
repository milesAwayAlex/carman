# 0033: On a shared, managed cluster: published images, secrets made in the cluster, a nightly copy to a bucket

Status: Accepted (2026-11-22); its nightly backup job superseded by 0038;
the harness's install at every job's start, in part, by 0041 (proposed).
Built in the chart; proved on the local cluster as far as that can be (see
Consequences). Not yet run on a managed cluster: the memory in the cloud is
postponed, and the local one is in use.

## Context

The chart was written for one small local cluster, where the deploy script
builds images straight into the runtime, makes the tokens, and the laptop
comes in through the cluster's load balancer. The owner decided to move the
memory service to a managed cluster shared with other workloads, deployed
by a GitOps controller from a repository of charts, reached from the laptop
over a private network, with backups to a bucket (the owner: a bucket is
fine; it does not have to be perfect from the start).

What such a cluster asks, found by asking about a real one: every pod under
the restricted pod security standard and with an account of its own; pods
placed on chosen nodes and labelled for cost; images pulled from a
registry; secrets not put in by a person at a terminal; and nothing
reachable from other namespaces merely because no policy says otherwise.

## Decision

- **Images are built by a workflow in this repository** for amd64 and arm64
  and pushed to the repository owner's registry. An installation names them
  in its values, by digest.
- **The harness is not in the published agent image; an agent installs it
  when it starts.** The harness is its vendor's software: its package says
  all rights reserved and names terms of use, and grants nobody the right
  to pass copies on. This repository's licence covers this repository's
  code and cannot cover that. An agent runs the vendor's own installer at
  start, on the stable channel unless told otherwise, and the harness then
  keeps itself up to date as it does on a person's machine (the owner:
  harnesses change daily; be less rigid than a pinned version). An image
  built by `scripts/deploy.sh` stays on the cluster it was built for and
  carries a copy, used only when the installer cannot be reached.
- **Every pod meets the restricted standard** (non-root, no privilege
  escalation, no capabilities, the runtime's default seccomp profile) **and
  has its own service account** with no token mounted.
- **`podLabels`, `nodeSelector` and `tolerations`** in the values apply to
  every pod; the labels also go on the library's volume.
- **`isolated`, on by default**: pods in the namespace accept connections
  only from one another. The local values turn it off, because there the
  laptop arrives through the cluster's load balancer.
- **The proxy reaches only public addresses** (`egress.closedTo`, the
  private, link-local and shared ranges by default). Agent pods reach only
  the proxy; without this they would reach, through it, everything the proxy
  can: other workloads and the cluster's private network. The local values
  close only the cluster's own ranges, because there the way out is the
  laptop's own and an agent may need its private routes. Found by a review
  of the first installation's chart, and shown on the local cluster: before,
  an agent pod got an answer from the cluster's API through the proxy;
  after, none.
- **Secrets by External Secrets Operator, where the cluster runs it**
  (`externalSecrets`). The role tokens are either read from the cluster's
  store or, with `memoryTokens.generate`, made in the cluster once and kept
  nowhere else; the worker's and dreamer's tokens then never leave it. The
  model credential is read from the store or left to be put in by a pipe.
- **A nightly backup job** (`backup`): it asks the service for the whole
  library as one file (the bundle of `cmem backup`), makes a library of it
  to prove it can be restored, and copies it to a bucket under a dated
  name. Its account takes a cloud identity by the cluster's workload
  identity, named in an annotation. This is the automatic backup that Q6
  left open; a git remote is no longer the plan.
- **Reaching the service from outside is not the chart's business yet.**
  The intent is a container of the private network's client inside the
  memory pod, so that the laptop's traffic arrives on loopback and the
  isolation above needs no opening. It gets its own decision when built.

## Consequences

- The local deploy is unchanged in what it does; it now builds the binary
  under a name that says its architecture, which is how one Dockerfile
  serves both.
- Not provable on the local cluster, and to be checked on the first managed
  one: the generated tokens (no operator locally), the upload (no bucket
  identity locally; the job was run up to that step), and workload identity
  with no token mounted.
- With generated tokens, deleting the secret makes new ones, and the laptop
  has to read its token again.
- The backup uses the agent image for the cloud provider's command-line
  tool. It knows one kind of destination (`gs://`); another provider is
  another case in `deploy/agent/backup-library`.
- Both images are public, as the repository is: the registry gives an image
  pushed from a public repository's workflow the repository's visibility.
- Every job downloads the harness when it starts, through the egress
  proxy: about half a minute and a few hundred megabytes. From the
  published image, an installer that cannot be reached fails the run before
  it begins.
- Two runs of one agent a week apart may use different versions of the
  harness. What ran is in the run's first lines.
