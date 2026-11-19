# 0028: carman holds its own credentials, as secrets in its cluster

Status: Proposed

## Context

Until now every credential reached carman's programs from the owner's
machine: a shell function read them from a password manager when a session
started, and later a keychain held a copy for test runs. That was set up in
the owner's machine configuration, which is a separate project, and a rule
recorded there says the model token is never written to a file, a repository
or a cluster secret.

An agent that runs on a cluster at night cannot read a laptop's keychain.
The owner's direction: stop leaning on the machine configuration; carman is
built for a cluster, local or in the cloud, and a running carman manages its
own secrets. A password manager or a cloud secret store may supply them.

The memory service already works this way for its own role tokens: they are
made at the first deploy and live in a Kubernetes Secret in its namespace.

## Decision

- **Every credential a carman workload uses is a Kubernetes Secret in
  carman's namespace**, named for what it is, and reaches a container as an
  environment variable or a file by reference. Nothing is baked into an
  image or written in a manifest.
- **Each secret has a source of record outside the cluster** (a password
  manager item, or an entry in a cloud secret store). The cluster holds a
  working copy.
- **Putting a copy in is an explicit act, by a pipe**:
  `SOURCE | scripts/deploy.sh CONTEXT secret NAME`. The value goes from the
  source's own command to the cluster's API on standard input. It is never
  an argument, never in a file, never printed.
- **The first is `carman-model`**: the credential the harness uses to call
  a model. The dreamer's schedule cannot be switched on until it exists.
- **A workload gets the narrowest credentials that do its job.** The
  dreamer gets the consolidation role's memory token and the model
  credential, and no others.

## Consequences

- This sets aside, for carman's cluster only, the rule that the model token
  is never in a cluster secret. Whoever can read secrets in the namespace
  can spend the owner's model usage. On the local cluster that is whoever
  has the laptop's kubeconfig.
- A subscription token made by the harness's own command can only make
  model requests. That limits what a leaked copy can do to spending usage,
  and is also why an agent holding it has none of the owner's connectors.
- Refreshing a secret is a person running one command. Nothing notices an
  expired one except the run that fails.
- Tools beyond memory need their own credentials, one secret per service,
  and those services have to offer something a headless agent can hold: a
  key, a service account, or a sign-in done once and stored.

## Open

- **Which source of record**, and whether the copy is made by hand or by an
  operator in the cluster that follows the source (External Secrets and the
  like). By hand is enough for one secret on one cluster.
- **Which model credential.** A subscription token is what exists. An API
  key from the provider's console is the usual thing for unattended
  workloads and can be capped and revoked separately.
- **Encryption at rest** in the cluster's store, which the local cluster
  does not have switched on.

## Alternatives considered

- **Start each run from the laptop and pass the token in.** No copy in the
  cluster, but the schedule is then the laptop's, which ADR 0027 rejects.
- **Mount the laptop's keychain copy.** A cluster cannot.
