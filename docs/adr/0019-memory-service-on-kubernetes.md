# 0019. The memory service runs on Kubernetes from the start, on the local cluster first

Status: Accepted (2026-11-18). Built and running on the local cluster. Its choice of plain manifests with kustomize overlays is superseded by ADR 0030.

## Context

To use the memory daily the owner needs the service always up. Two homes were
considered: a background service on the laptop, or the local cluster (k3s in a
Lima VM, context `devvm`). The owner chose the cluster: the VM is a utility
machine kept running for this, and the system's eventual home is Kubernetes
(ADR 0003), so the deployment is the same thing early. Later the owner expects
to turn this repository into a module for their machine configuration.

## Decision

- **One StatefulSet, one replica, one volume.** The library lives in the
  volume. One replica is a property of the service, not of the cluster.
- **Plain manifests with kustomize overlays**, applied by `scripts/deploy.sh
  CONTEXT`. The base is the same everywhere; an overlay says how the service
  is reached from outside. The context is always named.
- **The local overlay publishes a LoadBalancer on port 8377.** k3s's service
  load balancer serves it on the VM, and Lima forwards it to the laptop's
  loopback address, so the service is at `http://127.0.0.1:8377` there. The
  first deployment used the host's network instead, because at the time the VM
  forwarded neither a LoadBalancer nor a NodePort; that was a fault in the VM's
  configuration, fixed the same day.
- **The image is built into the cluster's own runtime.** On this VM `docker
  build` and k3s share one image store, so there is no registry.
- **Tokens are generated on the first deploy** and kept in a Secret. The
  script writes the laptop's and the consolidation role's settings to files
  only the owner can read, and prints no token.

## Consequences

- The laptop's access depends on Lima's port forwarding. The session hook
  reports an unreachable memory at session start, so a broken forward does not
  fail silently.
- There is no backup yet. The volume is on the VM's state disk, which outlives
  the VM's own disk. The local storage class deletes a volume with its claim,
  and a VM that is killed rather than shut down can lose the last writes. A
  git remote is the intended backup and is still open (Q6).
- This is the first workload on the VM that has to be up before a terminal is
  open, so the VM now needs to start with the machine and shut down cleanly.
  That belongs to the owner's machine configuration, not to this repository.
- The tokens are written to files only the owner can read. The owner's machine
  configuration keeps no secret on disk; when this becomes a module there, the
  laptop's token should be read from the password manager at launch instead.
- Benchmark runs keep using throwaway services on the host, not this one.
- The development service moves to port 8378 so it cannot be mistaken for, or
  collide with, the real one. The real library is write-once: a test note
  written to it by mistake stays in its history.
- On GCP the overlay will differ: a registry, a real storage class, and
  Tailscale in place of the host's network (ADR 0003, R4).
