# Running and recovering the memory service

How to start the service, check it, and get it back after something goes
wrong. Written for the local cluster: k3s in a VM on a laptop, context
`devvm` in the examples. There is no automation around any of this yet; these
are the steps by hand.

## Where everything is

| What | Where | Survives |
|---|---|---|
| The memory (the library, a git repository of notes) | Volume `data-cmem-0`, at `/data/library` in the pod | Pod and VM restarts; deleting the pod or the StatefulSet |
| The service | Pod `cmem-0`, namespace `carman` | Comes back by itself whenever the cluster is up |
| The tokens | Secret `cmem-tokens`; a copy wherever you keep secrets | Redeploys. Lost if the Secret is deleted |
| The image | The VM's image store, nowhere else | VM restarts. May be removed under disk pressure while no pod uses it |
| The way in from the laptop | `http://127.0.0.1:8377`, a port the VM forwards | Only while the VM runs |

## Is it working?

    scripts/deploy.sh devvm status      # what runs, and whether it answers from this machine
    scripts/deploy.sh devvm check       # is every note as it was written, and is git whole

`status` should end with "the service answers". `check` should say "The
library is intact" and "git finds the repository whole".

## Starting it up

After the laptop restarts, or whenever the VM was stopped:

1. Start the VM that runs the cluster (for a Lima VM named devvm:
   `limactl start devvm`). The cluster starts with it, and the pod with the
   cluster. Allow a minute.
2. `scripts/deploy.sh devvm status`.

Nothing else is needed. Sessions started while it was down were told at their
start that memory could not be reached; they recalled nothing and what they
learned was not recorded. Nothing else about them was affected.

If `status` does not end with "the service answers":

- **The pod is not Running.** `kubectl --context devvm -n carman describe pod
  cmem-0` says why near the end.
  - `ErrImageNeverPull` or `ImagePullBackOff`: the image is no longer in the
    VM's store. `nix develop -c scripts/deploy.sh devvm` builds it again. The
    memory and the tokens are not touched.
  - A volume that cannot be attached or found: see "The volume is gone".
- **The pod is Running but the laptop gets no answer.** The service is fine
  and the way in is not. Check that nothing else on the laptop holds the port
  (`lsof -nP -iTCP:8377 -sTCP:LISTEN`); the VM skips a forward whose port is
  taken. If the port is free, the VM's forwarding has gone stale: stop and
  start the VM.

## After an unexpected stop

Power loss, a killed VM, a laptop that crashed. Start it up as above. The
service puts right what it can as it starts:

- It removes the lock files git leaves when killed mid-write, which would
  otherwise make every later write fail.
- A note that was whole on disk but not yet committed is committed. The log
  says so: `kubectl --context devvm -n carman logs cmem-0 | grep unclean`.
- Every write is flushed to disk before it counts, so at most the one write
  in flight is lost. The agent making it got an error or no answer.

Then run `scripts/deploy.sh devvm check`. What it can report, and what to do:

- **Intact and whole.** Nothing to do.
- **A file that cannot be read as a note** ("no header block", "header block
  is not closed"). That is the write the stop cut short. Look at it and remove
  it, then restart the pod so the service reads the library afresh:

      kubectl --context devvm -n carman exec cmem-0 -- cat /data/library/PATH
      kubectl --context devvm -n carman exec cmem-0 -- rm /data/library/PATH
      kubectl --context devvm -n carman delete pod cmem-0

- **git does not find the repository whole** (an empty or corrupt object).
  The simplest way back is the last backup; see below. If there is none, the
  memory itself is not lost: every note is a plain file in the working tree,
  and git only holds their history. In the pod's shell
  (`kubectl ... exec -it cmem-0 -- sh`, then `cd /data/library`):

      find .git/objects -type f -empty -delete      # objects the stop left empty
      git fsck --strict --no-dangling                # whole now?

  If it still is not, keep the notes and start the history again. The
  history of who wrote what when is lost; no note is:

      mv .git ../library-git-broken
      git init --quiet --initial-branch=main
      git add -A && git -c user.name=cmem -c user.email=cmem@localhost commit --quiet -m "Start the history again after a damaged repository"

  Then restart the pod and run `check` again.

## Backing up

With `backup.enabled` in the values, a nightly job copies the library to
a store outside the cluster and reads the store back (ADR 0038;
`deploy/README.md` has the settings). A night that fails tells the owner,
where the installation has a command for that. What it has kept:

    scripts/deploy.sh devvm copies

Without a store, nothing does it for you. By hand:

    scripts/deploy.sh devvm backup cmem-library-$(date +%Y%m%d).tar

writes the whole library, history included, to a file on the laptop. Keep it
somewhere that is itself backed up, outside the VM's own disks, and outside
any directory an agent's pod mounts: an agent that can write there can
change or delete the copy. Do it before anything risky (rebuilding the VM,
changing the storage), and as often as you would mind losing what was
learned since. A scheduled copy should prove itself the same way the
nightly job does, by restoring what it made (`cmem restore FILE DIR`).

## Restoring a backup

    scripts/deploy.sh devvm restore --from-store          # the latest copy the nightly job kept
    scripts/deploy.sh devvm restore --from-store COPY     # the one with that id, from `copies`
    scripts/deploy.sh devvm restore cmem-library-20261119.tar
    scripts/deploy.sh devvm restore library.bundle        # a file made by `cmem backup`

`scripts/deploy.sh devvm copy NAME > FILE` gets any copy out as a file, the
library's or one of what else the installation has the job copy.

From the store, the copy passes through this machine in a file only you can
read, which is removed afterwards. If the store's own history had to be
used to bring a deleted repository back, run `restic unlock --remove-all`
and `restic repair index` against it first: what comes back includes the
locks and the removed copies of the time before.

The library in place is not deleted: it is moved aside in the same volume as
`/data/library.replaced-TIME`, and can be removed once you are satisfied. The
service restarts and reads the restored library. Notes written after the
backup was made are not in it.

## The volume is gone

Deleting the pod or the StatefulSet does not lose the memory; the volume
stays and the next deploy uses it. Deleting the volume's claim, the namespace,
or the VM's state disk does lose it, and so does a VM rebuilt from nothing.

1. Bring the cluster up, then `nix develop -c scripts/deploy.sh devvm`. This
   creates an empty library, and new tokens if the Secret went too.
2. If the nightly job's secret went too, put the repository's password and
   the store's keys back (`deploy/README.md`, Backup) and deploy again, so
   that the job exists to read the store through.
3. `scripts/deploy.sh devvm restore --from-store`, or `restore FILE` with
   the last backup made by hand.
4. If the tokens are new, the copies you keep elsewhere are now wrong. Store
   the new ones (`scripts/deploy.sh devvm token laptop | ...`, and the same
   for `dreamer`), or sessions will be told memory cannot be reached.

## Not covered yet

- Anything that tells you a night's backup never ran. A night that ran and
  failed does tell you.
- A copy of an interactive agent's home, or of the conversations jobs leave.
- Starting the VM with the laptop.
- Anything that tells you the service is down, other than the line a session
  shows when it starts.

## A copy that needs no cluster

Any machine that can reach the service, with any role's token, can take the
whole library away as one file and make a library of it somewhere else:

    cmem backup library.bundle
    cmem restore library.bundle DIR

The file is a git bundle: every note ever written, with its history.
`cmem restore` checks the copy and prints how to serve it on that machine,
which is what to do when the service is out of reach or there is no network:

    CMEM_TOKENS=laptop=SOME-TOKEN cmem serve --library DIR --listen 127.0.0.1:8378

and point sessions at it with `CMEM_URL` and `CMEM_TOKEN`. Notes written to
such a copy stay in it; bringing them back into the service is not built.
