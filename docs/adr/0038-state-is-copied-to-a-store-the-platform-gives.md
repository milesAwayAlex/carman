# 0038: The installation's state is copied to a store the platform gives, by a job of the chart

Status: Proposed (2026-11-22). Built; running on the local cluster against
a bucket that speaks S3, and a library restored from the bucket was the
same as the one in service. When accepted, it replaces the backup job of
ADR 0033.

## Context

ADR 0033 gave the chart a nightly job that copies the library to a bucket.
It knew one cloud's buckets and one cloud's command, so on a cluster on a
person's machine it stayed off, and the copy there was made by a job of
the machine's own configuration, outside this repository, into a directory
that agents' pods could also write to. Nothing copied anything but the
library.

The owner drew the line: what knows the state and how to copy and restore
it belongs to this project; what the machine or the cloud gives is the
place to keep copies. The same job should run on a commercial cloud with
only the address and the credential changed, and history of the copies is
wanted, because it undoes most mistakes.

Three ways of meeting that were weighed.

- **A cluster-wide backup tool** (one that copies volumes and runs hooks in
  pods). It needs installing for the whole cluster, with agents on every
  node that read other pods' volumes: on a shared cluster that is its
  owners' decision, not a chart's. And it copies a volume without knowing
  whether a library can be made of the copy.
- **Volume snapshots.** They need a storage driver that makes them, and on
  one machine they sit on the disk they protect.
- **A git remote for the library** (the earliest intention). It serves the
  library only, and needs a host for a repository that holds everything the
  owner's agents have learned.

## Decision

- **The platform gives a store; the chart does the rest.** What is asked of
  the platform is one thing: somewhere outside the cluster's own disks that
  restic can write to. A bucket that speaks S3 with a key for it, a cloud's
  own bucket with an identity for the job's account, or a directory. It
  should be out of reach of agents' pods, and keeping the store's own
  history of what is deleted from it is the platform's part.
- **The copies are a restic repository** (`backup.repository`). restic
  writes to all of those with one program, keeps each night as a snapshot,
  stores only what changed, encrypts before anything leaves the pod, and
  removes old copies by a rule the values give (`backup.keep`).
- **The repository's password is the one thing to keep outside the
  cluster**, with whoever would restore. It is in a secret the job reads
  (`backup.secret`), beside the store's keys where the store wants keys.
- **What is copied is a list, each entry a command whose output is the
  copy.** The library is always first: the job asks the memory service for
  the whole of it as one file and proves a library can be made of that file
  before keeping it. An installation names more under `backup.also`, such
  as a database's dump. A command that fails leaves no copy.
- **Every night ends by reading the store back** (restic's check), and a
  night in which anything failed tells the owner with the installation's
  command for that (ADR 0036), once.
- **Getting a copy back is the deploy script's**: `copies` lists what is
  kept, and `restore --from-store` puts the latest library, or a named one,
  in place of the library the way `restore FILE` does, moving the one in
  place aside. It reads the store from a pod made from the nightly job,
  because that is what can reach the store.
- **Not copied:** what the proxy keeps of people's sign-ins (a renewal
  token is used once, so a copy is stale the next hour and a risk until
  then), and secrets, which come from wherever the installation keeps them.

## Consequences

- restic has to be able to delete in the store, for its locks and for
  removing old copies. So the job's key can destroy the repository, and what
  stands against that is the store's own history (versions kept for a time)
  or the platform's copies of the store. A store with neither is one bug
  away from no backup.
- Losing the password loses every copy. Losing the cluster loses the secret
  that holds it, which is the day the copies are wanted.
- A copy is whole each night for the library (the file differs every time),
  so the repository grows by the library's size a night until old copies
  are removed. It is small.
- An interactive agent's home and the conversations jobs leave are not
  copied yet. They are volumes, not a command's output: copying them needs
  the job to mount them, on the node that has them. To be measured first.
- A night that never ran tells nobody; only a night that ran and failed
  does.
- The job is given the installation's commands, as agents are, and so the
  proxy's certificate; it is one more pod that can ask the proxy to speak
  for it.
