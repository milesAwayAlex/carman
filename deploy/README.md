# Deploying the memory service

The service runs on Kubernetes as one StatefulSet with one volume, which holds
the library (a git repository of markdown notes).

    nix develop -c scripts/deploy.sh devvm          build, apply, wait until it serves
    scripts/deploy.sh devvm status                  what is running, and whether it answers
    scripts/deploy.sh devvm env                     write the laptop's settings

The first argument is the kubectl context and is always given. `devvm` is the
local cluster.

## What gets created

In the namespace `carman`:

- `statefulset/cmem`, one replica. It must stay one: the service keeps the
  library in memory and serializes writes.
- `pvc/data-cmem-0`, 1 GiB. The library is at `/data/library` inside it.
- `service/cmem`, so that anything in the cluster reaches the memory at
  `http://cmem.carman.svc:8377`.
- `secret/cmem-tokens`, made on the first deploy: one random token each for
  the laptop, worker and consolidation roles. Later deploys leave it alone.

`deploy/k8s/base` is the same for every cluster. `deploy/k8s/local` is the
overlay for the local cluster; `CARMAN_OVERLAY` selects another.

## Reaching it from the laptop

The local cluster is k3s in a Lima VM. The local overlay makes the service a
LoadBalancer on port 8377. Lima forwards a LoadBalancer's port to the laptop,
so the laptop reaches the service at `http://127.0.0.1:8377`, on the loopback
address only. The forward appears a few seconds after the service does.

This relies on the VM's configuration as of 2026-11-18. Before that fix the
VM forwarded neither a LoadBalancer nor a NodePort, and the first deployment
here ran on the VM's own network instead.

`scripts/deploy.sh devvm env` writes two files, readable only by you:

- `.local/cmem.env`: `CMEM_URL` and the laptop role's `CMEM_TOKEN`. Source it
  in your shell profile, or copy the two values to wherever Claude Code gets
  its environment (see `adapters/claude-code/README.md`).
- `.local/cmem-dreamer.env`: the consolidation role's token, for
  `scripts/consolidate.sh`. Keep it out of ordinary sessions.

If the laptop cannot reach the service while `status` shows it running, look
at Lima's forwarding: something else on the laptop may hold port 8377 (Lima
then skips the forward and logs `failed to listen`), or the forward has gone
stale, as was seen on this machine for the API's own port. The VM's operating
manual covers both. The hook says at the start of a session when memory cannot
be reached, so this does not fail silently.

## Looking at the library

    kubectl --context devvm -n carman exec cmem-0 -- git -C /data/library log --oneline
    kubectl --context devvm -n carman logs cmem-0

## What this does not do yet

- **Back up.** The library is one volume. On the local cluster it sits on the
  VM's state disk, so it survives a rebuild of the VM and deleting the
  instance, but not deleting the state disk. The storage class deletes a
  volume when its claim is deleted: removing the StatefulSet or the pod keeps
  the claim, removing the claim or the namespace loses the memory. The service
  does not sync its writes to disk, so a VM that is killed rather than shut
  down can lose the last notes. A git remote to push to is the intended backup
  (DESIGN.md, question Q6).
- **Keep its image.** The image exists only in the VM's image store. While the
  pod runs it is safe; under disk pressure the kubelet may remove an image no
  pod is using. Deploying again rebuilds it.
- **Rotate tokens.** Delete the secret and deploy again, then store the new
  tokens wherever the old ones were kept.

## Keeping the tokens in a password manager

`env` writes the tokens to files only you can read. To keep them off the disk
instead, pipe each to the program that stores it:

    scripts/deploy.sh devvm token laptop | your-password-manager ...

`token` refuses to write to a terminal. A shell function can then read the
token when `claude` starts and pass it as `CMEM_TOKEN`, with the hook settings
from the flake's `cmem-claude-code-settings` package given to `claude
--settings`.
- **Deploy to a cluster with a registry.** Build and push the image yourself,
  set `CARMAN_IMAGE`, and add an overlay that says how the service is reached.
