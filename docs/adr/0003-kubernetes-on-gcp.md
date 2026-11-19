# 0003. Run on Kubernetes on GCP with workload identity

Status: Accepted (2026-11-17)

## Context

The system has to act on GCP, GitHub and external services. The owner already
works on GCP and prefers Kubernetes; a VM would also do.

## Decision

Run workers and services on Kubernetes on GCP. Workers get their cloud
permissions through workload identity and IAM bindings.

## Consequences

- One pod per task fits the rule that everything except memory is disposable.
- The agent's shell inherits the pod's identity. The IAM bindings on the
  worker's service account are therefore the real limit on what the agent can
  do, and should be scoped with that in mind.
