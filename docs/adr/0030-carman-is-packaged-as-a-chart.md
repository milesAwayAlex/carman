# 0030: carman is packaged as a Helm chart

Status: Accepted (2026-11-19). Built for the memory service and the dreamer.
Supersedes the part of ADR 0019 that chose plain manifests with kustomize
overlays.

## Context

ADR 0019 put the memory service on Kubernetes with plain manifests and a
kustomize overlay for each kind of cluster. That was right for one workload
on one local cluster. carman now has a scheduled agent beside the memory
service, with settings that differ by cluster and by taste (the schedule,
the ladder of models, the budget, which routines run), and it is headed for
a managed cluster where the owner's other workloads are deployed from
charts by a GitOps controller. The owner asked for a chart.

## Decision

- **One chart, `charts/carman`, holds everything carman runs on a cluster**:
  the memory service and, beside it, its agents. What differs between
  clusters is a values file under `deploy/values/`.
- **The chart contains no secret and makes none.** It names the secrets it
  expects (the memory's role tokens, the model credential), and those are
  made by `scripts/deploy.sh` or put there by a pipe (ADR 0028).
- **`scripts/deploy.sh` renders the chart and applies it**; it does not
  install a release. The local cluster keeps one way of working (build,
  apply, wait), and the objects already running were taken over without
  being recreated. A cluster with a GitOps controller or `helm install`
  consumes the same chart directly.
- **Resilience on a single node is fast restart, not a second copy.** The
  memory service stays one replica by design (ADR 0019): it recovers its
  library on start, sits on a volume that outlives the pod, and has probes.
  Jobs are bounded by deadlines and never overlap.

## Consequences

- Two tools are needed to deploy: `kubectl` and `helm`.
- A controller of carman's own is not part of this. A CronJob covers what
  runs on a schedule. Agents started by events will need something that
  turns an event into a Job, and that is the dispatcher's design.
- What is not in the chart yet and belongs there: the egress proxy that
  agents call out through, network policy for agent pods, a backup of the
  library that leaves the cluster, and a signal when a nightly job fails.

## Alternatives considered

- **Keep kustomize.** No templating of values such as the ladder or the
  list of routines without patches for each, and not what the destination
  cluster's tooling consumes.
- **Install a Helm release locally.** The running objects would have had to
  be adopted or recreated, for no gain on a cluster with one user.
