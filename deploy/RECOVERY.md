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

With `backup.enabled` in the values, a nightly job copies the library,
the dispatcher's database, the installation's definition, the
conversations jobs have kept and each interactive agent's sessions to a
store outside the cluster and reads the store back (ADR 0038;
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

## An interactive agent's home is lost

The nightly job keeps the agent's sessions as `sessions-NAME`: the
harness's transcripts, the list of sessions `agent-open` reopens, and the
harness's state and settings. Not its checkouts: those are cloned again,
and what was not pushed from them is gone. In the commands, CONTEXT and
NAMESPACE are the installation's, NAME the agent's.

1. Deploy, so that the agent's StatefulSet is there; its pod starts on a
   new, empty home (`home-agent-NAME-0`). If the old claim is still there
   and damaged, delete it first and then the pod
   (`scripts/deploy.sh CONTEXT agent restart NAME`).
2. Get the copy (the latest, or one with its id from `copies`):

       scripts/deploy.sh CONTEXT copies | grep sessions-NAME
       scripts/deploy.sh CONTEXT copy sessions-NAME [COPY] > sessions-NAME.tar
       tar -tvf sessions-NAME.tar | head          # a look before it goes back

   It is everything the agent's conversations hold: keep the file only as
   long as needed, where only you can read it.
3. Put it in the home, over what the new pod wrote at its start:

       kubectl --context CONTEXT -n NAMESPACE exec -i agent-NAME-0 -- tar -x -C /home/agent < sessions-NAME.tar

4. Check out again what the sessions work in. The directories are in the
   list, and a session whose directory is missing is not opened:

       kubectl --context CONTEXT -n NAMESPACE exec agent-NAME-0 -- agent-open

5. Restart the pod, which opens every remembered session on its
   conversation: `scripts/deploy.sh CONTEXT agent restart NAME`.

**Check:** `scripts/deploy.sh CONTEXT agent attach NAME`. Each session in
the list has a window of its name, and in it the harness shows the
conversation as it was at the copy, earlier turns above the prompt.
Without attaching, the windows are listed by
`kubectl ... exec agent-NAME-0 -- tmux -S /tmp/tmux-10001/default list-windows -t main`.
A conversation opened from an older copy than the newest one is fine: it
goes on from where the copy ends.

## The kept conversations are lost

With `agent.sessionStorage`, jobs keep their conversations on the volume
`agent-sessions`, and the nightly job copies it as `conversations`.

1. Deploy: the chart makes the claim again, empty.
2. `scripts/deploy.sh CONTEXT copy conversations [COPY] > conversations.tar`
3. Write it into the volume, from an interactive agent's pod, which mounts
   it at `/sessions`:

       kubectl --context CONTEXT -n NAMESPACE exec -i agent-NAME-0 -- tar -x -C /sessions < conversations.tar

   With no interactive agent, from a pod made for it (AGENT-IMAGE is the
   installation's `agent.image`):

       kubectl --context CONTEXT -n NAMESPACE run conversations-back --rm -i --restart=Never \
         --image=AGENT-IMAGE --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":10001,"runAsGroup":10001,"fsGroup":10001,"seccompProfile":{"type":"RuntimeDefault"}},
           "containers":[{"name":"conversations-back","image":"AGENT-IMAGE","stdin":true,"stdinOnce":true,"command":["tar","-x","-C","/sessions"],
             "securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}},
             "volumeMounts":[{"name":"s","mountPath":"/sessions"}]}],
           "volumes":[{"name":"s","persistentVolumeClaim":{"claimName":"agent-sessions"}}]}}' < conversations.tar

   One conversation alone: name it after the archive, as
   `AGENT/DIR/ID.jsonl` (`tar -tf conversations.tar | grep ID`).

**Check:** `scripts/deploy.sh CONTEXT agent resume` lists the kept
conversations with their agents; `scripts/deploy.sh CONTEXT agent resume
ID` opens one as its agent, with its earlier turns.

If `copy` or `copies` waits and then fails, the pod it reads the store from
could not start: it mounts the volumes the nightly job copies, and one of
them is missing or on another node (`kubectl ... get pvc`, and `describe`
the pod `backup-read-*`). Make the claim exist first, as above.

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

## A change broke it

A deploy can break what agents stand on: the egress proxy and its
credentials, the network rules, the start of an agent's pod, the
dispatcher's schema. An agent that builds this installation from inside the
cluster then cannot repair it, because it can no longer reach its model.
The way back is from outside, from a checkout on a machine that reaches the
cluster, and it is the same whoever made the change:

    scripts/deploy.sh CONTEXT status      # "deployed:" and "before it:" name two commits

Each deploy that ends with the service answering records in the cluster the
commit it was made from and the one before it, and the same for the
installation's own definition. To go back, deploy the one before:

    git worktree add ../CHECKOUT-back COMMIT-BEFORE && cd ../CHECKOUT-back
    ln -s PATH-TO/.local .local
    nix develop -c scripts/deploy.sh CONTEXT

The link gives the worktree the installation's definition and this
machine's settings; it is ignored by git, so the deploy is recorded as the
plain commit. If `status` shows that the installation's definition changed
too (its "before it" differs from what is deployed), put the definition
itself back first, in place, since the files beside it that are not tracked
are needed by the deploy and a second copy would lack them:

    git -C PATH-TO/.local checkout DEFINITION-BEFORE    # before the deploy
    git -C PATH-TO/.local checkout main                 # after it, whatever its result

Whoever runs the way back does both; nobody edits the definition in between.

Then check: `status` says the service answers and names the commit you
deployed, and the memory service, the proxy and the dispatcher are Running.
Tried from outside by a session that had no part in the change, on
2026-11-26: four minutes from the first command to the end of the deploy.

What this does not reach: a standing interactive agent keeps the pod it
has, on the image it started with, until that pod is deleted. If the change
broke the agent image, delete the pod after the deploy
(`scripts/deploy.sh CONTEXT agent restart NAME`), knowing that it ends the
sessions open in it; their conversations resume from disk.

A machine's container tool may print errors about credentials for
registries this installation does not use while it deploys. They do not
matter here: look at the deploy's own last lines and at `status`.

Images are named by their content, so the earlier ones are still in the
registry or the node's store and nothing is rebuilt unless they were
removed. Data is not rolled back by this: a dispatcher migration that ran
stays (an older build refuses a newer schema and says so; restore its
database from the store if the migration itself is the damage), and memory
is untouched by any deploy.

**Before a risky change** (anything in the list above, or to the rights of
whoever is deploying), the one making it tells someone who can run the way
back, a person or a session on a machine outside the cluster:

1. what is about to change, and what would show that it went wrong;
2. the commit to go back to (`status` prints it) and the commands above;
3. a time by which they will hear that it went well.

If that time passes with no word, the other side runs the way back without
being asked. The change is made only after they have said they have it.

## Not covered yet

- Anything that tells you a night's backup never ran. A night that ran and
  failed does tell you.
- A copy of what is in an interactive agent's checkouts. Its sessions are
  copied (below); work in a checkout that is not pushed is not.
- Copying conversations on a cluster of several nodes (`deploy/README.md`,
  Backup).
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
