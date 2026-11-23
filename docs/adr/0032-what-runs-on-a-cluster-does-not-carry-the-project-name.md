# 0032: What runs on a cluster does not carry the project's name

Status: Accepted (2026-11-22). Built. The owner's condition on accepting:
the rule is not to cost features; if it does, it is reconsidered.

## Context

carman is a framework anyone can use, and its repository says nothing of
any one installation (see the rules in `CLAUDE.md`). The owner added the
other half when the first installation outside the local cluster was being
prepared: on the cluster where it runs it is that installation's tool,
named as the installation likes, and the project's name does not appear
there. The two are kept apart in both directions.

The chart did not allow that. Secrets, config maps, a pod label, mounted
paths and the settings the routine runner reads all carried the name, so
they would have shown in the cluster and in any repository that keeps the
charts it deploys.

## Decision

- **Only `Chart.yaml` says what the chart is called.** The templates, the
  values, the images' own files and every object, label, path and
  environment variable they produce use plain names for what the thing is
  (`egress-credentials`, `model-credential`, `routines`,
  `app.kubernetes.io/component: agent`, `/etc/agent/...`). `scripts/test.sh`
  fails if the name comes back.
- **The routine runner's settings are named for the command that reads
  them**: `CMEM_ROUTINE_MODELS`, `CMEM_ROUTINE_BUDGET_USD` and the rest,
  beside the other `CMEM_` settings.
- **The namespace is the installer's choice.** The chart never names one;
  `scripts/deploy.sh` uses `carman` unless `CARMAN_NAMESPACE` says otherwise.
- **`scripts/chart-export.sh NAME DIR` writes the chart out under another
  name**, whole, for an installation that keeps its charts in a repository
  of its own. It refuses to write a copy in which the project's name
  appears.
- **The published images are `cmem` and `cmem-agent`**, after the binary
  they hold.
- Settings read on the machine that deploys or benchmarks (`CARMAN_IMAGE`,
  `CARMAN_VALUES`, `CARMAN_BENCH_*`) keep their names. They never reach a
  cluster.

## Consequences

- An installation made before this has to move: its secrets and config maps
  are copied to the new names before the deploy, and the old ones deleted
  after. The local cluster was moved this way.
- The binary still holds the module path of its source, and an image's
  registry page says where it was built. Hiding where the software comes
  from is not the aim; not naming it in someone else's cluster and
  repositories is.
- A vendored copy is a copy: it is brought up to date by exporting again,
  and what differs between the two is whatever the installation changed.
