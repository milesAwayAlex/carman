# 0051: The owner's token stays on the owner's machine, and the dispatcher knows only its hash

Status: Proposed (2026-11-27).

## Context

The dispatcher's `owner` role may do everything, and a message sent with
its token arrives in every session as the owner's word (ADR 0045, ADR
0049). Its token was in the cluster in three places: the secret the deploy
script makes (`dispatcher-tokens`, the key `owner` and inside
`DISPATCHER_TOKENS`), the dispatcher's environment, to check callers, and
the dispatcher's pod's `DISPATCHER_TOKEN`, which existed only so that
`scripts/deploy.sh CONTEXT dispatch ...` could run the client inside that
pod with `kubectl exec`. Whoever could read the namespace's secrets could
therefore write as the owner.

The owner, 2026-11-27: "i'm in favor of keeping the owner token off the
cluster, why would we need it there? let's make sure it's not where it's
not needed".

## Decision

**The service checks the owner's token against its hash.** The dispatcher
is given the SHA-256 of the owner's token (`DISPATCHER_OWNER_SHA256`, hex,
from the key `owner-sha256` of the token secret), not the token. It hashes
what a caller presents and compares the two in constant time. A plain hash
is enough: the token is 24 random bytes, so there is nothing to guess and
no need for a salt or a slow hash, which exist for passwords people choose.
The hash itself presented as a token is refused, as any wrong token is. The
service refuses to start if `DISPATCHER_TOKENS` holds an owner token, so a
secret in the old form cannot be used by mistake. The other roles' tokens
stay as they are: pods need those in the clear.

**The token lives on the owner's machine.** The deploy script makes it and
writes it to `.local/dispatcher-owner.token` (git-ignored, mode 600),
beside `.local/dispatcher.env`, and gives the cluster only the hash. It is
never printed. The secret is now created rather than applied, so that no
copy of its values is kept in an annotation.

**The owner's client runs on the owner's machine.** `scripts/deploy.sh
CONTEXT dispatch status|log|submit|peers|send|reply|inbox|read|message|watch|dump`
build the `dispatcher` client from the checkout and run it here, with the
token from that file, through the environment and never on a command line.
They reach the service at its LoadBalancer where it answers on this machine
(the local cluster, port 8379, as for the sessions of ADR 0049), and
otherwise through a `kubectl port-forward` for the length of the command.
They run as the owner, not as a session the shell may be in: the variables
that name a session are cleared for the client. `dispatch restore` uses the
same client to show the status after.

The other users of the dispatcher's tokens were checked: the dreamer and
agents use the `agent` role, the nightly backup the `backup` role, the
usage reading the `agent` role, and the sessions on the owner's machine the
`host` role. None used the owner's. The only one was the client inside the
dispatcher's pod, which is gone.

**Moving an installation.** A deploy that finds the key `owner` in the
secret makes a new owner token (the old one has been on the cluster),
writes the file, and replaces the secret with one holding the hash and
every other token as it was (the `host` token among them, which is already
installed on the owner's machine). Replacing it also drops the annotation in
which an earlier `apply` kept every value. The dispatcher restarts on the
new secret, and the old token stops working. This is done just before the
chart is applied, so a deploy that fails earlier (an image that does not
build) leaves the secret as the running dispatcher expects it.

A deploy that finds a hash in the cluster and no token on this machine, or
a token that is not the one the cluster knows, says so.
`scripts/deploy.sh CONTEXT dispatch owner-token new` makes a new token and
replaces the hash. Whoever has the rights to deploy may do this, as before
whoever had them could read the token.

**The way back.** A commit from before this one expects the token in the
clear. `scripts/deploy.sh CONTEXT dispatch owner-token back` puts the
token from the file back into the secret, as the key `owner` and in
`DISPATCHER_TOKENS`, and the older commit is then deployed
(`deploy/RECOVERY.md`). A later deploy of this commit moves it off again,
with a new token.

## What this closes, and what it does not

It closes one path: reading the namespace's secrets (or a copy of them, in
a backup of the cluster's store) no longer lets anyone write as the owner.
The hash in the secret does not.

It does not close these. Whoever may change the dispatcher's deployment or
its secret can put in the hash of a token of their own and restart it;
whoever may write to its database can forge any record, a message from the
owner included. The owner's authority is then the rights to deploy, as it
always was. Those rights are the owner's, and no agent's: agents have none
over the namespace.

Also not changed: the token travels from the owner's machine to the
service in plain HTTP over the local cluster's forwarded port, as the host
token does (ADR 0049), or inside the cluster API's TLS connection when the
port is forwarded. The old token may remain in the cluster store's history
until it is compacted; it no longer opens anything once the dispatcher has
restarted.

## Consequences

- The owner's commands need a Go toolchain on the owner's machine (the
  flake's, through `nix develop`, as the image builds already do), and on a
  cluster with no way in, the rights to forward a port.
- A second machine of the owner's needs a copy of the file, as it needs
  `.local/dispatcher.env`.
- `dispatch` commands no longer depend on the dispatcher's image having a
  client in it.
