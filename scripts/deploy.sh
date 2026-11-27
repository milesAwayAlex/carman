#!/usr/bin/env bash
# Deploys the memory service to a Kubernetes cluster, named by its kubectl
# context. The context is always given; this never acts on the default one.
#
#   scripts/deploy.sh CONTEXT           build the image, apply, wait until it serves
#   scripts/deploy.sh CONTEXT status    what is running, and whether it answers
#   scripts/deploy.sh CONTEXT env       write the laptop's settings to .local/cmem.env,
#                                       the consolidation role's to .local/cmem-dreamer.env
#                                       and the dispatcher's host role's to .local/dispatcher.env
#   scripts/deploy.sh CONTEXT check     ask the service's library whether every note is as written
#   scripts/deploy.sh CONTEXT backup FILE    copy the whole library, history included, to a tar file
#   scripts/deploy.sh CONTEXT restore FILE   put a backup in place of the library and restart
#   scripts/deploy.sh CONTEXT restore --from-store [COPY]   the same, with the latest copy the nightly job kept (or the one with that id)
#   scripts/deploy.sh CONTEXT copies         the copies the nightly job has kept
#   scripts/deploy.sh CONTEXT copy NAME [COPY] > FILE   one of them: the latest of NAME (library, or a name under backup.also), or the one with that id
#   scripts/deploy.sh CONTEXT definition apply MESSAGE-ID   apply to the installation's definition, as commits,
#                                       a change an agent proposed in a message (ADR 0056); deploys nothing
#                                       (--file FILE in place of the id: the message's text, saved)
#   scripts/deploy.sh CONTEXT definition put   put the definition's bundle in the cluster, without deploying
#   scripts/deploy.sh CONTEXT secrets   make the secrets a deploy makes, and move an old dispatcher
#                                       token secret, without deploying (for scripts/trial.sh)
#   scripts/deploy.sh CONTEXT token ROLE | ...
#                                       write one role's token to a pipe, for handing to a
#                                       password manager; refuses to write it to a terminal
#   ... | scripts/deploy.sh CONTEXT secret NAME [KEY]
#                                       put what arrives on standard input into the cluster as
#                                       the secret NAME (key KEY, or "token"); never an argument
#   scripts/deploy.sh CONTEXT dreamer on|off    let the nightly consolidation run, or stop it
#   scripts/deploy.sh CONTEXT dreamer run       run it once now and show what it reported
#   scripts/deploy.sh CONTEXT dreamer log       the report of the latest run
#   scripts/deploy.sh CONTEXT agent-image       build the agent image and print its name
#   scripts/deploy.sh CONTEXT warm run|log      make the warm image now, if it needs making;
#                                       or each kept run's line (ADR 0041)
#   scripts/deploy.sh CONTEXT routine FILE      put a routine of this installation's own in the cluster
#   scripts/deploy.sh CONTEXT tool FILE         put a command of this installation's own on agents' PATH
#   scripts/deploy.sh CONTEXT model-account      which account for the model the egress proxy holds, where the
#                                       installation has more than one (its definition lists them)
#   scripts/deploy.sh CONTEXT model-account NAME [--restart [--now]]   put that account's credential at the proxy,
#                                       wait until its meter has it (or restart the proxy: --now even
#                                       while jobs run), record the choice, and take a usage reading on it
#   scripts/deploy.sh CONTEXT signin KEY URL    sign in, in a browser, to a service a renewal keeps a sign-in for
#   scripts/deploy.sh CONTEXT agent run NAME    run an agent once now and show what it reported
#   scripts/deploy.sh CONTEXT agent log NAME    the report of its latest run
#   scripts/deploy.sh CONTEXT agent attach NAME attach this terminal to an interactive agent's session
#   scripts/deploy.sh CONTEXT agent restart NAME restart an interactive agent, onto the image last deployed;
#                                       whatever runs in it ends, and conversations resume from disk
#   scripts/deploy.sh CONTEXT agent resume      the conversations agents' jobs have kept, and where each is open
#   scripts/deploy.sh CONTEXT agent resume ID   go to the session with that conversation, wherever it is: a
#                                       job's goes on as the agent that had it, in a pod of its own
#                                       (the same again attaches to that pod); a standing agent's
#                                       is attached to at its window
#   scripts/deploy.sh CONTEXT dispatch status [ID]   what the dispatcher runs, what waits and why, the last runs
#   scripts/deploy.sh CONTEXT dispatch log [--type T] [--after N] [--last N]   its log
#   scripts/deploy.sh CONTEXT dispatch submit CLASS [ID] [--priority N] ...   start a run of the CronJob CLASS
#                                       through the dispatcher (ADR 0042)
#   scripts/deploy.sh CONTEXT dispatch peers|send|reply|inbox|read|message|watch ...   messages,
#                                       as the owner (ADR 0045): who can be written to, a message
#                                       to a session, an agent or the owner, the owner's inbox
#   scripts/deploy.sh CONTEXT dispatch improve [list|show ID|agree ID|decline ID 'WHY'|note ...]
#                                        the list of what should be improved in the platform (ADR 0058)
#   scripts/deploy.sh CONTEXT dispatch dump > FILE      its database, as SQL
#   scripts/deploy.sh CONTEXT dispatch restore FILE     put a dump in place of its database, which is kept aside
#   scripts/deploy.sh CONTEXT dispatch owner-token new  make a new owner token, here, and give the cluster its hash
#   scripts/deploy.sh CONTEXT dispatch owner-token back put the owner token back on the cluster, for going back
#                                       to a commit from before ADR 0051 (deploy/RECOVERY.md)
#
# The dispatch commands run the dispatcher's client built from this checkout,
# on this machine, as the owner, with the token in .local/dispatcher-owner.token
# (ADR 0051). The cluster holds only that token's SHA-256.
#
# What is applied is the chart in charts/carman, rendered with the values in
# deploy/values/NAME.yaml; NAME is `local` unless CARMAN_VALUES names another.
# CMEM_URL is where this machine reaches the service, and is
# http://127.0.0.1:8377 unless set, which is right for the local values.
# Needs kubectl and helm.
#
# The image is built with `docker build`. On the local cluster (context devvm)
# that builds straight into the cluster's container runtime, so no registry is
# needed. For a cluster that pulls from a registry, push the image yourself
# and set CARMAN_IMAGE to its name.
#
# Tokens are made once, on the first deploy, and live only in the cluster's
# secret and in the files `env` writes. They are never printed. The
# dispatcher's owner token is the exception: it lives only on this machine.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "deploy: $*" >&2; exit 1; }

# Local, git-ignored settings, such as where build files go on this machine.
if [ -f .local/bench.env ]; then
    set -a
    # shellcheck disable=SC1091
    . .local/bench.env
    set +a
fi

context="${1:-}"
[ -n "$context" ] || die "name the kubectl context to deploy to, for example: scripts/deploy.sh devvm"
action="${2:-deploy}"
# The namespace everything lives in; CARMAN_NAMESPACE names another.
namespace="${CARMAN_NAMESPACE:-carman}"
# CARMAN_OBJECT_PREFIX: a prefix on the names of the objects this script
# reads and makes, for a trial whose objects carry its name (ADR 0054).
pre="${CARMAN_OBJECT_PREFIX:-}"
kube=(kubectl --context "$context" --namespace "$namespace")
# CARMAN_KUBE_SERVER overrides the API address, for reaching it through a tunnel.
[ -z "${CARMAN_KUBE_SERVER:-}" ] || kube+=(--server "$CARMAN_KUBE_SERVER")
values="deploy/values/${CARMAN_VALUES:-local}.yaml"
[ -f "$values" ] || die "no values file at $values"
# What belongs to one installation and not to the repository: its agents,
# the cloud identity they take. Git-ignored, and optional.
chart_values=(-f "$values")
[ ! -f .local/values.yaml ] || chart_values+=(-f .local/values.yaml)
url="${CMEM_URL:-http://127.0.0.1:8377}"

answers() { curl -fsS -m 3 -o /dev/null "$url/healthz" 2>/dev/null; }

# Runs backup-state with the given arguments where the nightly job runs: in
# a pod made from that job, which is what can reach the store and read it.
from_store() {
    "${kube[@]}" get cronjob backup >/dev/null 2>&1 || die "this installation keeps no nightly copies (backup.enabled)"
    local pod="backup-read-$(date +%H%M%S)" status=0
    "${kube[@]}" create job "$pod" --from=cronjob/backup --dry-run=client -o json |
        "${kube[@]}" patch --local -f - --type json -o json \
            -p '[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sleep","900"]}]' |
        "${kube[@]}" create -f - >/dev/null || die "could not start a pod to read the store"
    "${kube[@]}" wait --for=condition=ready pod -l "job-name=$pod" --timeout=120s >/dev/null || status=1
    [ "$status" -ne 0 ] || "${kube[@]}" exec "job/$pod" -- backup-state "$@" || status=1
    "${kube[@]}" delete job "$pod" --wait=false >/dev/null
    return "$status"
}

secret() { "${kube[@]}" get secret ${pre}cmem-tokens -o "jsonpath={.data.$1}" | base64 -d; }

# Waits until a Job has finished one way or the other, and says which.
finished() {
    local job="$1" limit="$2" state="" waited=0
    while [ "$waited" -lt "$limit" ]; do
        state="$("${kube[@]}" get "job/$job" -o 'jsonpath={.status.conditions[?(@.status=="True")].type}' 2>/dev/null)"
        case "$state" in *Complete*) return 0 ;; *Failed*) return 1 ;; esac
        sleep 5
        waited=$((waited + 5))
    done
    return 1
}

# The build context has to be somewhere the container runtime can see.
build_root="${CARMAN_BENCH_ROOT:-$HOME/carman-bench}/build"
# CARMAN_REGISTRY names a registry the cluster pulls from (HOST:PORT). With
# it, what is built here is pushed there and pods name it there, so that a
# node that removes an image to make room can get it back. Without it the
# images exist only in the runtime they were built into.
registry="${CARMAN_REGISTRY:+${CARMAN_REGISTRY%/}/}"
publish() {
    [ -z "$registry" ] || docker push -q "$1" >/dev/null 2>&1 || die "could not push $1 to the registry"
}
# Builds the binary into the directory given, named for the architecture
# the cluster's runtime builds for, which is how the Dockerfiles take it.
linux_cmem() {
    local arch build=(go build)
    arch="$(docker info --format '{{.Architecture}}')"
    case "$arch" in aarch64 | arm64) arch=arm64 ;; x86_64 | amd64) arch=amd64 ;; *) die "unknown architecture $arch" ;; esac
    command -v go >/dev/null || build=(nix develop -c go build)
    rm -f "$1"/cmem-*
    # The same source gives the same file, and so the same image: without
    # this the commit is written into it, and every commit restarts the pods.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "${build[@]}" -buildvcs=false -trimpath -o "$1/cmem-$arch" ./cmd/cmem
    # The agent image carries the dispatcher's client as well.
    [ "${2:-}" != with-dispatcher ] ||
        CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "${build[@]}" -buildvcs=false -trimpath -o "$1/dispatcher-$arch" ./cmd/dispatcher
}

# The dispatcher's image (deploy/dispatcher). Sets DISPATCHER_IMAGE unless
# it is already set to an image pushed elsewhere.
build_dispatcher() {
    [ -z "${DISPATCHER_IMAGE:-}" ] || return 0
    local root="$build_root/dispatcher" arch build=(go build)
    mkdir -p "$root"
    arch="$(docker info --format '{{.Architecture}}')"
    case "$arch" in aarch64 | arm64) arch=arm64 ;; x86_64 | amd64) arch=amd64 ;; *) die "unknown architecture $arch" ;; esac
    command -v go >/dev/null || build=(nix develop -c go build)
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "${build[@]}" -buildvcs=false -trimpath -o "$root/dispatcher-$arch" ./cmd/dispatcher
    cp deploy/dispatcher/Dockerfile "$root/"
    DISPATCHER_IMAGE="${registry}carman-dispatcher:$(cat "$root/dispatcher-$arch" "$root/Dockerfile" | shasum -a 256 | cut -c1-12)"
    docker image inspect "$DISPATCHER_IMAGE" >/dev/null 2>&1 || docker build -q --label built-by=deploy -t "$DISPATCHER_IMAGE" "$root" >/dev/null ||
        die "the dispatcher's image did not build"
    publish "$DISPATCHER_IMAGE"
    echo "built $DISPATCHER_IMAGE" >&2
}

# The installation's definition, where it is a git repository of its own
# (CARMAN_INSTALLATION; .local unless set).
installation="${CARMAN_INSTALLATION:-.local}"
# What this script writes beside the definition that holds a credential, or
# is this machine's alone: never part of the definition.
definition_never='cmem.env cmem-dreamer.env dispatcher.env dispatcher-owner.token bench.env'
# Puts a bundle of the definition, history included, in the cluster (the
# config map `installation`): the nightly backup keeps it, and an agent
# given it reads it (ADR 0056). Only what is committed is in a bundle, on
# any branch. Fails where there is no definition, or none was put.
put_definition() {
    [ -e "$installation/.git" ] && git -C "$installation" rev-parse HEAD >/dev/null 2>&1 || return 1
    local bundle never="" f ref
    # A file of this machine's that was committed by mistake would go to
    # the cluster, and to every agent given the definition: refused.
    for ref in $(git -C "$installation" for-each-ref --format='%(refname)') ; do
        for f in $definition_never; do
            [ -z "$(git -C "$installation" log --format=%h -1 "$ref" -- "$f" 2>/dev/null)" ] || never="$never $f(${ref#refs/})"
        done
    done
    if [ -n "$never" ]; then
        echo "deploy: the installation's definition has these of this machine's own in its history, so no copy of it is put in the cluster:$never" >&2
        return 1
    fi
    bundle="$(mktemp)"
    # Branches and tags, not --all: a stash is a ref too, and holds what was not committed.
    if git -C "$installation" bundle create "$bundle" HEAD --branches --tags >/dev/null 2>&1 && [ "$(wc -c < "$bundle")" -lt 900000 ] &&
        "${kube[@]}" create configmap ${pre}installation "--from-file=definition.bundle=$bundle" --dry-run=client -o json |
        "${kube[@]}" replace --force -f - >/dev/null 2>&1; then
        rm -f "$bundle"
        echo "the cluster holds a copy of the installation's definition, as of $(git -C "$installation" log -1 --format='%h %s' | cut -c1-70)"
        [ -z "$(git -C "$installation" status --porcelain)" ] ||
            echo "deploy: the installation's definition has changes that are not committed; they are deployed, and the copy kept is of the last commit" >&2
        return 0
    fi
    rm -f "$bundle"
    echo "deploy: no copy of the installation's definition was put in the cluster (a bundle over 900 kB does not fit a config map, or the cluster refused it); the backup goes on without it" >&2
    return 1
}

# Applies a change to the definition that an agent proposed in a message
# (agent-definition propose, ADR 0056), as the commits it sent: author and
# message kept, with the message it came in. Refused whole if it does not
# apply as it is, or touches what a definition's change may not. Deploys
# nothing.
apply_definition() {  # MESSAGE-ID
    local id="$1" dir from base was commit path n=0 bad="" look=""
    [ -e "$installation/.git" ] || die "the installation's definition at $installation is not a git repository"
    [ -z "$(git -C "$installation" status --porcelain --untracked-files=no)" ] || die "the definition at $installation has changes that are not committed; commit them or put them aside first"
    dir="$(mktemp -d)"
    if [ "$id" = --file ]; then
        # The message's text, saved: for a machine that does not reach the dispatcher.
        [ -f "${2:-}" ] || { rm -rf "$dir"; die "name the file that holds the message's text"; }
        cp "$2" "$dir/message"
        id="in-a-file" from="a-file"
    else
        dispatch_client message "$id" --body > "$dir/message" || { rm -rf "$dir"; die "no message $id came from the dispatcher"; }
        from="$(dispatch_client message "$id" --from)"
    fi
    base="$(sed -n 's/^-----BEGIN DEFINITION PATCH----- base=//p' "$dir/message" | head -1)"
    sed -n '/^-----BEGIN DEFINITION PATCH-----/,/^-----END DEFINITION PATCH-----$/p' "$dir/message" | sed '1d;$d' > "$dir/mbox"
    [ -s "$dir/mbox" ] || { rm -rf "$dir"; die "message $id holds no proposed change (no patch between its BEGIN and END lines)"; }
    mkdir "$dir/patches"
    git mailsplit -o"$dir/patches" "$dir/mbox" >/dev/null
    for path in "$dir"/patches/*; do
        # Who wrote a commit is its own word; who sent it is the dispatcher's.
        git interpret-trailers --in-place --trailer "Proposed-in: message $id, from ${from%% *}" "$path"
    done
    # Tried first where it changes nothing: a checkout of the definition's
    # commit beside it. What the commits touch is then read from git's own
    # trees, not from the patch's text.
    was="$(git -C "$installation" rev-parse HEAD)"
    git -C "$installation" worktree add -q --detach "$dir/tried" "$was"
    undo() { git -C "$installation" worktree remove --force "$dir/tried" >/dev/null 2>&1 || true; rm -rf "$dir"; }
    if ! git -C "$dir/tried" -c core.hooksPath=/dev/null am --quiet "$dir"/patches/* >/dev/null 2>&1; then
        undo
        die "message $id does not apply to the definition as it is ($(git -C "$installation" log -1 --format='%h %s' | cut -c1-60)); it was made against ${base:0:12}, and nothing was applied. Ask its sender to bring the clone up to date (agent-definition update) and propose it again"
    fi
    for commit in $(git -C "$dir/tried" rev-list --reverse "$was..HEAD"); do
        n=$((n + 1))
        while IFS= read -r -d '' path; do
            case "$path" in
            .git | .git/* | */.git | */.git/* | .gitignore | */.gitignore | .gitattributes | */.gitattributes | .gitmodules | */.gitmodules)
                bad="$bad $path(git's own)" ;;
            model-accounts) look="$look model-accounts(commands that run on this machine)" ;;
            tools/*) look="$look $path(a command agents run)" ;;
            routines/*) look="$look $path(what an agent is told)" ;;
            esac
            for never in $definition_never; do [ "$path" != "$never" ] || bad="$bad $path(this machine's own)"; done
            # What the definition's own rules leave out is not the definition's.
            ! git -C "$installation" check-ignore -q --no-index -- "$path" || bad="$bad $path(ignored by the definition)"
            # A link, or a repository inside it, is not a file of the definition.
            case "$(git -C "$dir/tried" ls-tree "$commit" -- "$path" | cut -c1-6)" in 120000 | 160000) bad="$bad $path(a link or a repository)" ;; esac
            # What the definition tracks may change; a new file only where
            # routines and commands are kept. Anything else new (a file this
            # script would read, one that is here and not tracked) is for
            # the owner's side to add.
            if ! git -C "$installation" ls-files --error-unmatch -- "$path" >/dev/null 2>&1; then
                case "$path" in
                routines/* | tools/*) [ ! -e "$installation/$path" ] || bad="$bad $path(here already, and not the definition's)" ;;
                *) bad="$bad $path(new, and not a routine or a command)" ;;
                esac
            fi
        done < <(git -C "$dir/tried" diff-tree --no-commit-id --name-only -r --no-renames -z "$commit")
    done
    if [ -n "$bad" ]; then
        undo
        die "message $id touches what a change to the definition may not, and nothing was applied:$bad"
    fi
    new="$(git -C "$dir/tried" rev-parse HEAD)"
    git -C "$installation" merge -q --ff-only "$new" || { undo; die "the definition could not be moved to the proposed commits; nothing was applied"; }
    undo
    echo "applied $n commit(s) from message $id (from $from) to the definition at $installation:"
    git -C "$installation" log --format='  %h %an: %s' "$was..HEAD"
    git -C "$installation" diff --stat "$was" HEAD | sed 's/^/  /'
    [ -z "$look" ] || echo "Look closely at:$look"
    echo "Nothing is deployed. Read it (git -C $installation log -p ${was:0:12}..HEAD), then deploy: scripts/deploy.sh $context. To take it back: git -C $installation reset --hard ${was:0:12}"
}

# The dispatcher's owner token (ADR 0051): on this machine only, readable
# by this user only. The cluster holds its SHA-256.
# CARMAN_OWNER_TOKEN_FILE names another, for a trial installation's own.
owner_token_file="${CARMAN_OWNER_TOKEN_FILE:-.local/dispatcher-owner.token}"

# The accounts for the model that this installation has (model-account):
# NAME, then the command that prints its credential, one to a line, in the
# installation's definition; and the one chosen, a word in a file there.
model_secret="${CARMAN_MODEL_SECRET:-${pre}model-credential}"
model_accounts_file="${CARMAN_INSTALLATION:-.local}/model-accounts"
model_account_file="${CARMAN_INSTALLATION:-.local}/model-account"
model_accounts() { [ -s "$model_accounts_file" ] || die "this installation lists no accounts for the model: $model_accounts_file has a line for each, NAME then the command that prints its credential"; }
model_account_command() { awk -v n="$1" '$1 == n {sub(/^[^ \t]+[ \t]+/, ""); print; exit}' "$model_accounts_file"; }
# The account whose credential the proxy's secret holds, by comparing
# digests; nothing, if it is none of those listed.
model_account_at_proxy() {
    local held name command
    held="$("${kube[@]}" get secret "$model_secret" -o 'jsonpath={.data.token}' 2>/dev/null | base64 -d | tr -d '\n' | sha256_hex)"
    while read -r name command; do
        case "$name" in "" | \#*) continue ;; esac
        if [ "$(sh -c "$command" 2>/dev/null | tr -d '\n' | sha256_hex)" = "$held" ]; then echo "$name"; return 0; fi
    done < "$model_accounts_file"
}
sha256_hex() { shasum -a 256 | cut -d' ' -f1; }
# Makes a new owner token and writes it to the file; prints its hash.
new_owner_token() {
    local token
    token="$(openssl rand -hex 24)"
    mkdir -p .local
    (umask 077 && printf '%s\n' "$token" > "$owner_token_file.new") && mv "$owner_token_file.new" "$owner_token_file" ||
        die "could not write $owner_token_file"
    printf '%s' "$token" | sha256_hex
}
owner_token_hash() { tr -d '\n' < "$owner_token_file" | sha256_hex; }
# The hash the cluster holds, or nothing.
cluster_owner_hash() { "${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.data.owner-sha256}' 2>/dev/null | base64 -d; }
cluster_has_owner_token() { [ -n "$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.data.owner}' 2>/dev/null)" ]; }

# Runs the dispatcher's client, built from this checkout, on this machine,
# as the owner (ADR 0051): against the service's LoadBalancer where it
# answers here, else through a port forwarded for the length of the command.
dispatch_client() {
    "${kube[@]}" get deployment ${pre}dispatcher >/dev/null 2>&1 || die "this installation runs no dispatcher (dispatcher.enabled)"
    [ -s "$owner_token_file" ] || die "no owner token at $owner_token_file: deploy, or make a new one with: scripts/deploy.sh $context dispatch owner-token new"
    local bin="$build_root/dispatcher-client" build=(go build) port durl="" forward="" log="" status=0
    mkdir -p "$build_root"
    command -v go >/dev/null || build=(nix develop -c go build)
    CGO_ENABLED=0 "${build[@]}" -o "$bin" ./cmd/dispatcher || die "the dispatcher's client did not build"
    port="$("${kube[@]}" get service ${pre}dispatcher -o 'jsonpath={.spec.ports[0].port}')"
    if [ "$("${kube[@]}" get service ${pre}dispatcher -o 'jsonpath={.spec.type}')" = LoadBalancer ]; then
        durl="${DISPATCHER_URL:-http://127.0.0.1:$port}"
        curl -fsS -m 3 -o /dev/null "$durl/healthz" 2>/dev/null || durl=""
    fi
    if [ -z "$durl" ]; then
        log="$(mktemp)"
        "${kube[@]}" port-forward service/${pre}dispatcher ":$port" > "$log" 2>&1 &
        forward=$!
        for _ in $(seq 50); do
            durl="$(sed -n 's|^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*|http://127.0.0.1:\1|p' "$log" | head -1)"
            [ -z "$durl" ] && kill -0 "$forward" 2>/dev/null || break
            sleep 0.2
        done
        [ -n "$durl" ] || { kill "$forward" 2>/dev/null; cat "$log" >&2; rm -f "$log"; die "could not forward a port to the dispatcher"; }
    fi
    # As the owner, not as a session this shell may be in; the token goes
    # through the environment, never a command line.
    (
        unset DISPATCHER_SESSION CLAUDE_CODE_SESSION_ID DISPATCHER_PLACE
        export DISPATCHER_IDENTITY=/dev/null DISPATCHER_URL="$durl"
        DISPATCHER_TOKEN="$(tr -d '\n' < "$owner_token_file")"
        export DISPATCHER_TOKEN
        exec "$bin" "$@"
    ) || status=$?
    # (wait gives the killed forward's status, which is not the command's)
    [ -z "$forward" ] || { kill "$forward" 2>/dev/null; wait "$forward" 2>/dev/null || true; rm -f "$log"; }
    return "$status"
}

# The egress proxy answers in the name of each host it holds a credential
# for. Its certificate comes from an authority made here, once, whose public
# half agent pods trust. The certificate is issued again only when the list
# of hosts changes. Key files exist for the length of this function, in a
# directory only this user can read.
egress_restart=0
egress_certs() {
    local hosts="$1" dir alt="" host
    dir="$(mktemp -d)" && chmod 700 "$dir"
    # shellcheck disable=SC2064
    trap "rm -rf '$dir'" RETURN
    if ! "${kube[@]}" get secret egress-ca >/dev/null 2>&1; then
        printf '[req]\ndistinguished_name=dn\nx509_extensions=v3\nprompt=no\n[dn]\nCN=egress proxy\n[v3]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign\n' > "$dir/ca.cnf"
        openssl ecparam -name prime256v1 -genkey -noout -out "$dir/ca.key"
        openssl req -new -x509 -key "$dir/ca.key" -out "$dir/ca.crt" -sha256 -days 3650 -config "$dir/ca.cnf"
        "${kube[@]}" create secret generic egress-ca "--from-file=ca.crt=$dir/ca.crt" "--from-file=ca.key=$dir/ca.key" >/dev/null
        "${kube[@]}" create configmap egress-ca "--from-file=ca.crt=$dir/ca.crt" --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
        echo "made the egress proxy's certificate authority"
    fi
    [ "$("${kube[@]}" get secret egress-tls -o 'jsonpath={.metadata.annotations.egress/hosts}' 2>/dev/null)" != "$hosts" ] || return 0
    "${kube[@]}" get secret egress-ca -o 'jsonpath={.data.ca\.crt}' | base64 -d > "$dir/ca.crt"
    "${kube[@]}" get secret egress-ca -o 'jsonpath={.data.ca\.key}' | base64 -d > "$dir/ca.key"
    for host in $hosts; do alt="$alt${alt:+,}DNS:$host"; done
    printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$alt" > "$dir/tls.ext"
    openssl ecparam -name prime256v1 -genkey -noout -out "$dir/tls.key"
    openssl req -new -key "$dir/tls.key" -out "$dir/tls.csr" -subj "/CN=${hosts%% *}"
    openssl x509 -req -in "$dir/tls.csr" -CA "$dir/ca.crt" -CAkey "$dir/ca.key" -CAcreateserial -out "$dir/tls.crt" -sha256 -days 825 -extfile "$dir/tls.ext" 2>/dev/null
    # Made anew, not applied: an applied secret keeps a second copy of the key in its annotations.
    "${kube[@]}" delete secret egress-tls --ignore-not-found >/dev/null
    "${kube[@]}" create secret generic egress-tls "--from-file=tls.crt=$dir/tls.crt" "--from-file=tls.key=$dir/tls.key" >/dev/null
    "${kube[@]}" annotate secret egress-tls "egress/hosts=$hosts" --overwrite >/dev/null
    egress_restart=1
    echo "issued the egress proxy's certificate for: $hosts"
}

# The agent image: the harness, the memory client and the routines. Sets
# CARMAN_AGENT_IMAGE unless it is already set to an image pushed elsewhere.
build_agent() {
    [ -z "${CARMAN_AGENT_IMAGE:-}" ] || return 0
    local root="$build_root/agent"
    rm -rf "$root" && mkdir -p "$root"
    linux_cmem "$root" with-dispatcher
    cp deploy/agent/Dockerfile deploy/agent/run-routine deploy/agent/usage-reading deploy/agent/backup-state deploy/agent/warm-harness deploy/agent/agent-env deploy/agent/agent-session deploy/agent/agent-attach deploy/agent/agent-resume deploy/agent/agent-open deploy/agent/agent-definition deploy/agent/agent-view deploy/agent/agent-wake deploy/agent/child-harness deploy/agent/oauth-credential deploy/agent/attention.md deploy/agent/renew-credential deploy/agent/settings.json "$root/"
    cp -R routines "$root/routines"
    # A new tag whenever anything in it changes.
    # CARMAN_AGENT_BASE names an image to build on in place of the plain one:
    # an installation's own shell and tools (deploy/agent/Dockerfile).
    local base=()
    [ -z "${CARMAN_AGENT_BASE:-}" ] || base=(--build-arg "BASE=$CARMAN_AGENT_BASE")
    CARMAN_AGENT_IMAGE="${registry}carman-agent:$( (cd "$root" && find . -type f | sort | xargs shasum -a 256; echo "${CARMAN_AGENT_BASE:-}") | shasum -a 256 | cut -c1-12)"
    # A node removes images that nothing runs, and the base is one of those:
    # CARMAN_AGENT_BASE_BUILD is the installation's command that makes it again.
    if [ -n "${CARMAN_AGENT_BASE:-}" ] && [ -n "${CARMAN_AGENT_BASE_BUILD:-}" ] &&
        ! docker image inspect "$CARMAN_AGENT_IMAGE" >/dev/null 2>&1 && ! docker image inspect "$CARMAN_AGENT_BASE" >/dev/null 2>&1; then
        echo "the base image $CARMAN_AGENT_BASE is gone; making it again" >&2
        $CARMAN_AGENT_BASE_BUILD >/dev/null || die "$CARMAN_AGENT_BASE_BUILD did not make the base image"
    fi
    docker image inspect "$CARMAN_AGENT_IMAGE" >/dev/null 2>&1 || docker build -q --label built-by=deploy "${base[@]}" -t "$CARMAN_AGENT_IMAGE" "$root" >/dev/null || {
        echo "deploy: the agent image did not build; the build again, with its output:" >&2
        docker build --label built-by=deploy "${base[@]}" -t "$CARMAN_AGENT_IMAGE" "$root" >&2 || true
        exit 1
    }
    publish "$CARMAN_AGENT_IMAGE"
    echo "built $CARMAN_AGENT_IMAGE" >&2
}

# Attaches this terminal to the tmux session of an agent's pod. Where this
# attach comes from: the tmux pane it runs in, or else the terminal. A later
# attach from the same place replaces the client this one leaves in the pod
# if its connection is dropped.
attach() {
    [ -t 0 ] || die "attach needs a terminal"
    local from
    from="$(hostname -s):$(tty 2>/dev/null | tr '/' '_')"
    [ -z "${TMUX:-}" ] || from="$(hostname -s):tmux:$(tmux display-message -p '#{b:socket_path}:#{session_name}:#{window_index}.#{pane_index}' 2>/dev/null)"
    exec "${kube[@]}" exec -it "$1" -- env AGENT_TERM="${TERM:-}" COLORTERM="${COLORTERM:-truecolor}" AGENT_ATTACH_ID="$from" agent-attach
}

# The pod a Job has made, once it has made one.
job_pod() {
    local pod="" waited=0
    while [ -z "$pod" ] && [ "$waited" -lt 60 ]; do
        pod="$("${kube[@]}" get pods -l "job-name=$1" -o 'jsonpath={.items[0].metadata.name}' 2>/dev/null || true)"
        [ -n "$pod" ] || { sleep 1; waited=$((waited + 1)); }
    done
    [ -n "$pod" ] || die "job $1 made no pod"
    echo "$pod"
}

# Runs deploy/agent/agent-resume, with the given arguments, where every
# agent's kept conversations can be read: in a pod made for the purpose, with
# that volume read-only and no credential, removed after. It is this
# checkout's copy of the script, so it does not wait for an image.
conversations() {
    "${kube[@]}" get pvc agent-sessions >/dev/null 2>&1 || die "this installation keeps no conversations (agent.sessionStorage)"
    local job="conversations-$(date +%H%M%S)" status=0
    "${kube[@]}" create job "$job" --from=cronjob/dreamer --dry-run=client -o json |
        "${kube[@]}" patch --local -f - --type json -o json -p '[
            {"op":"add","path":"/metadata/ownerReferences","value":null},
            {"op":"add","path":"/spec/activeDeadlineSeconds","value":300},
            {"op":"add","path":"/spec/ttlSecondsAfterFinished","value":0},
            {"op":"add","path":"/spec/template/metadata/labels/app","value":"conversations"},
            {"op":"add","path":"/spec/template/spec/containers/0/command","value":["sh","-c","trap \"exit 0\" TERM; sleep 300 & wait"]},
            {"op":"add","path":"/spec/template/spec/containers/0/args","value":null},
            {"op":"add","path":"/spec/template/spec/containers/0/env","value":[]},
            {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"sessions","mountPath":"/sessions","readOnly":true}]},
            {"op":"add","path":"/spec/template/spec/volumes","value":[{"name":"sessions","persistentVolumeClaim":{"claimName":"agent-sessions","readOnly":true}}]}]' |
        "${kube[@]}" create -f - >/dev/null || die "could not start a pod to read the kept conversations"
    "${kube[@]}" wait --for=condition=ready "pod/$(job_pod "$job")" --timeout=120s >/dev/null || status=1
    [ "$status" -ne 0 ] || "${kube[@]}" exec -i "job/$job" -- sh -s -- "$@" < deploy/agent/agent-resume || status=$?
    "${kube[@]}" delete job "$job" --wait=false >/dev/null
    return "$status"
}

# A kept conversation goes on as the agent that had it (ADR 0040): in a pod
# made from that agent's own definition, its CronJob, with the same image,
# environment, credentials, mounts and account; the harness on the
# conversation in tmux. Leaving the harness and its shell ends the pod, and
# the cluster removes it. A pod is found again by its labels.
resume() {
    local want="$1" open pod agent id where job
    # The pods open on a conversation now: NAME, CONVERSATION.
    open="$("${kube[@]}" get pods -l conversation --field-selector=status.phase!=Succeeded,status.phase!=Failed \
        -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.conversation}{"\n"}{end}')"
    if [ -z "$want" ]; then
        conversations
        return
    fi
    case "$want" in *[!0-9a-f-]*) die "an id is hex digits and dashes, for example 58a6d1a8" ;; esac
    pod="$(printf '%s\n' "$open" | awk -F '\t' -v w="$want" 'NF == 2 && index($2, w) == 1 {print $1}')"
    [ "$(printf '%s' "$pod" | grep -c .)" -le 1 ] || die "more than one pod has a conversation whose id starts with $want: $(echo $pod)"
    if [ -z "$pod" ]; then
        # A session of a standing agent: its conversation is in that pod's
        # home, and its window in the pod's tmux carries the session's name,
        # which the harness writes into the conversation. Go to that window.
        local p window
        for p in $("${kube[@]}" get pods -l 'app.kubernetes.io/component=agent,statefulset.kubernetes.io/pod-name' \
            --field-selector=status.phase=Running -o 'jsonpath={.items[*].metadata.name}'); do
            window="$("${kube[@]}" exec -i "$p" -- sh -s -- "$want" 2>/dev/null <<'IN_POD' || true
f="$(ls -t "$HOME"/.claude/projects/*/"$1"*.jsonl 2>/dev/null | head -1)"
[ -n "$f" ] || exit 0
name="$(grep -o '"customTitle":"[^"]*"' "$f" | tail -1 | cut -d'"' -f4)"
t() { "$(cat /tmp/agent-tmux 2>/dev/null || echo tmux)" -S "/tmp/tmux-$(id -u)/default" "$@"; }
if [ -n "$name" ] && t list-windows -t main -F '#{window_name}' 2>/dev/null | grep -qxF "$name"; then
    t select-window -t "main:=$name" && echo "open $name"
else
    echo "closed ${name:-?}"
fi
IN_POD
)"
            case "$window" in
            "open "*)
                echo "the conversation is session ${window#open } of $p; attaching to it there"
                attach "$p" ;;
            "closed "*)
                die "the conversation is session ${window#closed } of $p, which is not open there. Attach (scripts/deploy.sh $context agent attach $(printf %s "$p" | sed 's/^agent-//; s/-0$//')) and open it: agent-open ${window#closed }" ;;
            esac
        done
    fi
    if [ -n "$pod" ]; then
        echo "$pod has the conversation open; attaching to it"
    else
        IFS=$'\t' read -r agent id where < <(conversations --find "$want") || exit 1
        [ -n "$id" ] || exit 1
        [ -z "$where" ] || die "$agent's conversation $id is open already, in $where. Leave it there first."
        # A harness in an interactive agent's pod that has it open and holds
        # no lock: started by hand, or by agent-resume from before the lock.
        for p in $("${kube[@]}" get pods -l 'app.kubernetes.io/component=agent,statefulset.kubernetes.io/pod-name' \
            --field-selector=status.phase=Running -o 'jsonpath={.items[*].metadata.name}'); do
            ! "${kube[@]}" exec "$p" -- pgrep -f -- "--resume.*$id" >/dev/null 2>&1 ||
                die "$agent's conversation $id is open already, in a harness in pod $p. Leave it there first."
        done
        local cronjob="agent-$agent"
        [ "$agent" != dreamer ] || cronjob=dreamer
        "${kube[@]}" get cronjob "$cronjob" >/dev/null 2>&1 || die "$agent is no longer an agent here (no cronjob/$cronjob); its conversation stays on the volume"
        job="resume-$(printf %s "$agent" | cut -c1-30)-$(printf %s "$id" | cut -c1-8)-$(date -u +%H%M%S)"
        # The pod runs this checkout's agent-resume (--hold), handed to it
        # whole, so that a resume does not wait for an image to carry it.
        # Not the CronJob's own Job: an open session must not hold up the
        # agent's schedule, and has no deadline. Its labels and its start
        # are what will let an idle one be found and ended.
        "${kube[@]}" create job "$job" "--from=cronjob/$cronjob" --dry-run=client -o json |
            "${kube[@]}" patch --local -f - --type json -o json -p '[
                {"op":"add","path":"/metadata/ownerReferences","value":null},
                {"op":"add","path":"/metadata/labels","value":{"conversation":"'"$id"'","agent":"'"$agent"'"}},
                {"op":"add","path":"/spec/activeDeadlineSeconds","value":null},
                {"op":"add","path":"/spec/backoffLimit","value":0},
                {"op":"add","path":"/spec/ttlSecondsAfterFinished","value":0},
                {"op":"add","path":"/spec/template/metadata/labels/conversation","value":"'"$id"'"},
                {"op":"add","path":"/spec/template/metadata/labels/agent","value":"'"$agent"'"},
                {"op":"add","path":"/spec/template/metadata/annotations","value":{"started":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'"}},
                {"op":"add","path":"/spec/template/spec/containers/0/command","value":["sh","-c","printf %s \"$0\" | base64 -d > /tmp/agent-resume && exec sh /tmp/agent-resume --hold \"$1\" \"$2\"","'"$(base64 < deploy/agent/agent-resume | tr -d '\n')"'","'"$id"'","'"$agent"'"]},
                {"op":"add","path":"/spec/template/spec/containers/0/args","value":null}]' |
            "${kube[@]}" create -f - >/dev/null || die "could not make a pod for $agent's conversation"
        pod="$(job_pod "$job")"
        echo "made $pod, as $agent, for its conversation $id; waiting until it can be attached to"
    fi
    local waited=0 phase
    until "${kube[@]}" exec "$pod" -- test -f /tmp/agent-session-ready 2>/dev/null; do
        phase="$("${kube[@]}" get pod "$pod" -o 'jsonpath={.status.phase}' 2>/dev/null || echo gone)"
        case "$phase" in
        Succeeded | Failed | gone)
            "${kube[@]}" logs "$pod" --tail=20 >&2 2>/dev/null || true
            die "the pod ended before it could be attached to"
            ;;
        esac
        [ "$waited" -lt 300 ] || die "$pod is not ready after five minutes; see: kubectl --context $context -n $namespace logs $pod"
        sleep 3
        waited=$((waited + 3))
    done
    "${kube[@]}" logs "$pod" 2>/dev/null | grep '^agent-resume: ' | tail -1 || true
    [ -t 0 ] || { echo "ready; attach from a terminal: scripts/deploy.sh $context agent resume $want"; return 0; }
    attach "$pod"
}

# The warm image (ADR 0041): runs its job once now, and says what it did in
# its own line. What it makes is for the next pods; a failure stops nothing.
warm_run() {
    "${kube[@]}" get cronjob agent-warm >/dev/null 2>&1 || die "this installation makes no warm image (agent.warm.enabled)"
    local job="agent-warm-now-$(date -u +%Y%m%d%H%M%S)"
    "${kube[@]}" create job "$job" --from=cronjob/agent-warm >/dev/null || die "could not start the warm image's job"
    echo "the warm image's job is running ($job); it takes a minute or two when it rebuilds"
    if finished "$job" 900; then
        "${kube[@]}" logs "job/$job" 2>/dev/null | grep '^warm: ' | tail -1
    else
        "${kube[@]}" logs "job/$job" --tail=5 2>/dev/null || true
        echo "the warm image was not made; agents start from the image they had, and install the harness if it has none" >&2
        return 1
    fi
}

# Wraps an object's data, as kubectl prints it, into a patch that changes
# only the data.
only_data() { printf '{"data":'; cat; printf '}'; }

# The secrets a deploy makes when they are missing: the memory's role
# tokens, the dispatcher's, and its database's password.
make_secrets() {
    if ! "${kube[@]}" get secret ${pre}cmem-tokens >/dev/null 2>&1; then
        laptop="$(openssl rand -hex 24)" worker="$(openssl rand -hex 24)" dreamer="$(openssl rand -hex 24)"
        # Through standard input, so the tokens are never on a command line.
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${pre}cmem-tokens
  namespace: $namespace
stringData:
  CMEM_TOKENS: "laptop=$laptop,worker=$worker,dreamer=$dreamer"
  laptop: "$laptop"
  worker: "$worker"
  dreamer: "$dreamer"
EOF
        echo "made the token secret"
    fi
    # The dispatcher's tokens and its database's password: random values,
    # through standard input, never printed. The password is made even for
    # a database the installation gives, where it is replaced by its own.
    # The owner's token is not among them: it is written on this machine,
    # and the cluster gets its hash (ADR 0051).
    if ! "${kube[@]}" get secret ${pre}dispatcher-tokens >/dev/null 2>&1; then
        agent="$(openssl rand -hex 24)" backup="$(openssl rand -hex 24)" host="$(openssl rand -hex 24)"
        owner_sha256="$(new_owner_token)"
        # Created, not applied: an applied secret keeps a copy of its values in an annotation.
        "${kube[@]}" create -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${pre}dispatcher-tokens
  namespace: $namespace
stringData:
  DISPATCHER_TOKENS: "agent=$agent,backup=$backup"
  owner-sha256: "$owner_sha256"
  agent: "$agent"
  backup: "$backup"
  host: "$host"
EOF
        echo "made the dispatcher's token secret, and the owner's token in $owner_token_file"
    elif [ -z "$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.data.host}')" ]; then
        # A secret made before the owner's machine had a role (ADR 0049).
        # The host token is a key of its own, not in DISPATCHER_TOKENS,
        # which a build from before it would refuse.
        host="$(openssl rand -hex 24)"
        "${kube[@]}" patch secret ${pre}dispatcher-tokens --type merge --patch-file /dev/stdin >/dev/null <<EOF
{"stringData": {"host": "$host"}}
EOF
        dispatcher_restart=1
        echo "added the host role's token to the dispatcher's token secret"
    fi
    if ! "${kube[@]}" get secret ${pre}dispatcher-db >/dev/null 2>&1; then
        openssl rand -hex 24 | tr -d '\n' | "${kube[@]}" create secret generic ${pre}dispatcher-db --from-file=password=/dev/stdin >/dev/null
        echo "made the dispatcher's database password"
    fi
}

# Moves a dispatcher token secret from before ADR 0051 off the owner's
# token; sets dispatcher_restart when it did.
move_owner_token() {
    # A secret from before ADR 0051 holds the owner's token. It has been on
    # the cluster, so a new one is made here, and the secret is replaced by
    # one with its hash and every other token as it was. Replaced, not
    # patched: that also drops the copy an apply kept in an annotation.
    # Done just before the apply, which restarts the dispatcher on it.
    if cluster_has_owner_token; then
        tokens_data() { "${kube[@]}" get secret ${pre}dispatcher-tokens -o "jsonpath={.data.$1}"; }
        kept="$(tokens_data DISPATCHER_TOKENS | base64 -d | tr ',' '\n' | { grep -v '^ *owner=' || true; } | paste -sd, -)"
        [ -n "$kept" ] || die "the dispatcher's token secret holds no token but the owner's; not moving it"
        agent_data="$(tokens_data agent)" backup_data="$(tokens_data backup)" host_data="$(tokens_data host)"
        [ -n "$agent_data" ] && [ -n "$backup_data" ] && [ -n "$host_data" ] || die "the dispatcher's token secret lacks agent, backup or host; not moving it"
        owner_sha256="$(new_owner_token)"
        "${kube[@]}" replace -f - >/dev/null <<EOF || die "could not replace the dispatcher's token secret; it is as it was, and $owner_token_file holds a token the cluster does not know"
apiVersion: v1
kind: Secret
metadata:
  name: ${pre}dispatcher-tokens
  namespace: $namespace
type: Opaque
stringData:
  DISPATCHER_TOKENS: "$kept"
  owner-sha256: "$owner_sha256"
data:
  agent: "$agent_data"
  backup: "$backup_data"
  host: "$host_data"
EOF
        dispatcher_restart=1
        echo "moved the owner's token off the cluster: a new one is in $owner_token_file, and the cluster holds its hash (ADR 0051)"
    elif [ -n "$(cluster_owner_hash)" ] && [ ! -s "$owner_token_file" ]; then
        echo "deploy: the cluster holds the hash of an owner token this machine does not have ($owner_token_file); to make a new one: scripts/deploy.sh $context dispatch owner-token new" >&2
    elif [ -s "$owner_token_file" ] && [ "$(cluster_owner_hash)" != "$(owner_token_hash)" ]; then
        echo "deploy: the owner token in $owner_token_file is not the one the cluster knows; to make a new one: scripts/deploy.sh $context dispatch owner-token new" >&2
    fi
}

case "$action" in
deploy)
    [ -z "$pre" ] || die "the chart is applied with its own names; a trial with a prefix applies it with scripts/trial.sh"
    if [ -z "${CARMAN_IMAGE:-}" ]; then
        root="$build_root/service"
        mkdir -p "$root"
        rm -f "$root/cmem"
        linux_cmem "$root"
        cp deploy/Dockerfile "$root/Dockerfile"
        # A new tag whenever the binary changes, so the cluster rolls it out.
        CARMAN_IMAGE="${registry}carman-cmem:$(cat "$root"/cmem-* "$root/Dockerfile" | shasum -a 256 | cut -c1-12)"
        docker build -q --label built-by=deploy -t "$CARMAN_IMAGE" "$root" >/dev/null
        publish "$CARMAN_IMAGE"
        echo "built $CARMAN_IMAGE"
    fi

    "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $namespace
EOF
    make_secrets
    build_dispatcher

    build_agent
    # Whether the dreamer's schedule is on is a setting of the cluster, not of
    # the manifest: a deploy leaves it as it was, and off the first time.
    # One the dispatcher starts is always suspended, and keeps it in an annotation (ADR 0059).
    dreaming="$("${kube[@]}" get cronjob dreamer -o 'jsonpath={.metadata.annotations.dispatcher/scheduled}' 2>/dev/null || true)"
    case "$dreaming" in
    on) dreaming=false ;;
    off) dreaming=true ;;
    *) dreaming="$("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.suspend}' 2>/dev/null || true)" ;;
    esac
    # What the egress proxy is to hold a credential for, from the values.
    # Images with a registry: a warm image (agent.warm) refuses one without.
    egress="$(helm template carman charts/carman --namespace "$namespace" "${chart_values[@]}" --set memory.image=registry.invalid/m:0 --set agent.image=registry.invalid/a:0 --set dispatcher.image=registry.invalid/d:0 --show-only templates/egress.yaml)" ||
        die "the egress proxy's settings do not render; see above"
    egress_hosts="$(printf '%s\n' "$egress" | sed -n 's/^  hosts: "\(.*\)"$/\1/p')"
    egress_keys="$(printf '%s\n' "$egress" | sed -n 's/^  keys: "\(.*\)"$/\1/p')"
    if [ -n "$egress_hosts" ]; then
        for key in $egress_keys; do
            [ -n "$("${kube[@]}" get secret egress-credentials -o "jsonpath={.data.$key}" 2>/dev/null)" ] ||
                die "the values name a credential '$key' for the egress proxy, and the cluster has none: ... | scripts/deploy.sh $context secret egress-credentials $key"
        done
        egress_certs "$egress_hosts"
    fi

    # Rendered and applied, not installed as a release: the cluster's objects
    # are the record, and a chart consumer elsewhere (a GitOps controller,
    # `helm install`) uses the same chart its own way.
    # The renewals beside the proxy run in the agent image, and the proxy's
    # pod restarts when their image changes, which every agent then feels.
    # They keep the image they have unless what they run has changed.
    renewal_source="$(cat deploy/agent/renew-credential deploy/agent/oauth-credential | shasum -a 256 | cut -c1-12)-${CARMAN_AGENT_BASE:-plain}${registry:+-$registry}"
    renewal_image="$CARMAN_AGENT_IMAGE"
    if [ "$("${kube[@]}" get deployment egress -o 'jsonpath={.metadata.annotations.renewal-source}' 2>/dev/null || true)" = "$renewal_source" ]; then
        running="$("${kube[@]}" get deployment egress -o 'jsonpath={.spec.template.spec.containers[?(@.name!="envoy")].image}' 2>/dev/null | cut -d' ' -f1)"
        [ -z "$running" ] || renewal_image="$running"
    fi
    # The installation's definition (its values, routines and commands),
    # where it is kept as a git repository of its own: a bundle of it,
    # history included, goes into the cluster, and the nightly backup keeps
    # it with the rest. CARMAN_INSTALLATION names the directory; .local
    # unless set. What is not committed is deployed all the same, and is
    # not in the copy.
    ! put_definition || chart_values+=(--set backup.installation=true)
    rendered="$(helm template carman charts/carman --namespace "$namespace" "${chart_values[@]}" \
        --set "memory.image=$CARMAN_IMAGE" --set "agent.image=$CARMAN_AGENT_IMAGE" --set "dispatcher.image=$DISPATCHER_IMAGE" --set "egress.renewalImage=$renewal_image" --set "dreamer.suspend=${dreaming:-true}" \
        --set "dispatcher.notices.context=$context")" ||
        die "the chart did not render"
    # With a warm image, agents' jobs start from a tag the warm job moves.
    # One made on the agent image this deploy replaces would start them with
    # that image's scripts: point the tag at the new image first, so the
    # first pods after a deploy start from it (and install the harness), and
    # make the warm one at the end.
    # (awk reads to the end: one that stops early fails the pipe.)
    warm_ref="$(printf '%s\n' "$rendered" | awk '$1 == "image:" {image = $2} $1 == "imagePullPolicy:" && $2 == "Always" && found == "" {found = image} END {print found}')"
    if [ -n "$warm_ref" ]; then
        warm_base="$(printf '%s\n' "$rendered" | awk '$3 == "WARM_BASE" {getline; base = $2} END {print base}' | tr -d '"')"
        if [ "$("${kube[@]}" get cronjob agent-warm -o 'jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].env[?(@.name=="WARM_BASE")].value}' 2>/dev/null || true)" != "$warm_base" ]; then
            docker image inspect "$CARMAN_AGENT_IMAGE" >/dev/null 2>&1 || docker pull -q "$CARMAN_AGENT_IMAGE" >/dev/null ||
                die "could not get $CARMAN_AGENT_IMAGE to point $warm_ref at it"
            docker tag "$CARMAN_AGENT_IMAGE" "$warm_ref" && docker push -q "$warm_ref" >/dev/null 2>&1 ||
                die "could not point $warm_ref at $CARMAN_AGENT_IMAGE"
            echo "pointed $warm_ref at $CARMAN_AGENT_IMAGE until the warm image is made"
        fi
    fi
    move_owner_token
    # The account for the model that the installation's definition chooses
    # is the one at the proxy; a deploy says when it is not, and changes nothing.
    if [ -s "$model_accounts_file" ] && [ -s "$model_account_file" ] && "${kube[@]}" get secret "$model_secret" >/dev/null 2>&1; then
        at_proxy="$(model_account_at_proxy)"
        [ "$at_proxy" = "$(cat "$model_account_file")" ] ||
            echo "deploy: the proxy holds ${at_proxy:-a credential of no listed account}, and the installation's definition chooses $(cat "$model_account_file"); to put it there: scripts/deploy.sh $context model-account $(cat "$model_account_file")" >&2
    fi
    printf '%s\n' "$rendered" | kubectl --context "$context" apply -f -   # every object names its namespace; one is not this one
    # Applying does not remove what the values no longer ask for.
    [ -n "$warm_ref" ] || "${kube[@]}" delete cronjob/agent-warm networkpolicy/warm-job-reaches-the-registry serviceaccount/agent-warm --ignore-not-found >/dev/null
    # The meter's right to read pods, where the proxy no longer holds the
    # model's credential (agent.modelCredential pod, ADR 0050).
    # (grep reads to the end: one that stops early fails the pipe.)
    printf '%s\n' "$rendered" | grep '^  name: egress-meter$' >/dev/null ||
        "${kube[@]}" delete rolebinding/egress-meter role/egress-meter --ignore-not-found >/dev/null
    "${kube[@]}" rollout status statefulset/${pre}cmem --timeout=120s
    if "${kube[@]}" get deployment ${pre}dispatcher >/dev/null 2>&1; then
        ! "${kube[@]}" get statefulset ${pre}dispatcher-db >/dev/null 2>&1 || "${kube[@]}" rollout status statefulset/${pre}dispatcher-db --timeout=180s
        # Its tokens are read when it starts.
        [ "${dispatcher_restart:-0}" = 0 ] || "${kube[@]}" rollout restart deployment/${pre}dispatcher >/dev/null
        "${kube[@]}" rollout status deployment/${pre}dispatcher --timeout=180s
    fi
    if "${kube[@]}" get deployment egress >/dev/null 2>&1; then
        "${kube[@]}" annotate deployment egress "renewal-source=$renewal_source" --overwrite >/dev/null
        # A new certificate is read when the proxy starts.
        [ "$egress_restart" = 0 ] || "${kube[@]}" rollout restart deployment/egress >/dev/null
        "${kube[@]}" rollout status deployment/egress --timeout=120s
    fi
    sleep 3 # a port forward to this machine, if there is one, takes a moment
    if answers; then
        echo "the service answers at $url"
    else
        echo "the service is running in the cluster but does not answer at $url from this machine;"
        echo "see deploy/README.md, \"Reaching it from the laptop\""
    fi
    # What is deployed, and what was before it, kept in the cluster: the way
    # back from a change that broke something is to deploy the one before
    # (deploy/RECOVERY.md). A commit is named only when nothing was changed
    # beside it; the installation's own definition has its commit too.
    now="$(git rev-parse HEAD 2>/dev/null || echo unknown)$([ -z "$(git status --porcelain 2>/dev/null)" ] || echo " with changes not committed")"
    theirs=""
    [ ! -e "${CARMAN_INSTALLATION:-.local}/.git" ] || theirs="$(git -C "${CARMAN_INSTALLATION:-.local}" rev-parse HEAD 2>/dev/null)$([ -z "$(git -C "${CARMAN_INSTALLATION:-.local}" status --porcelain 2>/dev/null)" ] || echo " with changes not committed")"
    was="$("${kube[@]}" get configmap deployed -o 'jsonpath={.data.commit}' 2>/dev/null || true)"
    was_theirs="$("${kube[@]}" get configmap deployed -o 'jsonpath={.data.installation}' 2>/dev/null || true)"
    if [ "$was" != "$now" ] || [ "$was_theirs" != "$theirs" ]; then
        "${kube[@]}" create configmap deployed --from-literal="commit=$now" --from-literal="installation=$theirs" \
            --from-literal="at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" --from-literal="previous=$was" --from-literal="previous-installation=$was_theirs" \
            --dry-run=client -o json | "${kube[@]}" apply -f - >/dev/null || echo "deploy: what was deployed could not be recorded in the cluster" >&2
        # And in the dispatcher's log (ADR 0058), with the subjects of the
        # commits since the one before: what reads the log for what should
        # be improved then knows what is merged and deployed, where a
        # message only names a branch. Only for a commit with nothing
        # changed beside it; not recording it never fails a deploy.
        if [ "${now#* }" = "$now" ] && [ "$now" != unknown ] && [ "$was" != "$now" ] && [ -s "$owner_token_file" ] && "${kube[@]}" get deployment dispatcher >/dev/null 2>&1; then
            range="$now"
            ! git merge-base --is-ancestor "${was%% *}" "$now" 2>/dev/null || range="${was%% *}..$now"
            (git log --format='%h %s' --max-count=200 "$range" | dispatch_client deployed "$now" ${was:+--previous "${was%% *}"}) ||
                echo "deploy: what was deployed could not be recorded in the dispatcher's log" >&2
        fi
    fi
    # Every deploy that changes a binary makes a new image tag. Where images
    # are built straight into the cluster's runtime, the old ones only fill
    # its disk: remove those this deploy did not use and no pod names (an
    # interactive agent keeps its image until it is restarted). One that a
    # finished Job still names is left by the runtime, and that is fine.
    in_use=" $("${kube[@]}" get pods -o 'jsonpath={.items[*].spec.containers[*].image} {.items[*].spec.initContainers[*].image}' 2>/dev/null) "
    for old in $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E '^([^/]+/)?carman-(cmem|agent|dispatcher):' || true); do
        [ "$old" = "$CARMAN_IMAGE" ] || [ "$old" = "$CARMAN_AGENT_IMAGE" ] || [ "$old" = "$DISPATCHER_IMAGE" ] || [ "${in_use#* $old }" != "$in_use" ] || docker rmi "$old" >/dev/null 2>&1 || true
    done
    # The cluster's runtime keeps a record of its own for an image it has
    # run, which outlives the tag: remove those of this script's builds too.
    # Only what carries this script's label, and never by force: what a
    # container still uses is refused, and other images are not looked at.
    for old in $(docker images --filter dangling=true -q 2>/dev/null | sort -u); do
        [ "$(docker image inspect "$old" --format '{{index .Config.Labels "built-by"}}' 2>/dev/null | tail -1)" = deploy ] || continue
        docker rmi "$old" >/dev/null 2>&1 || true
    done
    [ -z "$warm_ref" ] || warm_run || true
    ;;
warm)
    case "${3:-}" in
    run) warm_run ;;
    log)
        # One line from each run the cluster still keeps: the data for how
        # often the image needs making.
        for pod in $("${kube[@]}" get pods -l app=agent-warm --sort-by=.metadata.creationTimestamp -o 'jsonpath={.items[*].metadata.name}'); do
            "${kube[@]}" logs "$pod" 2>/dev/null | grep '^warm: ' | tail -1
        done
        ;;
    *) die "say: warm run, or warm log" ;;
    esac
    ;;
status)
    "${kube[@]}" get statefulset,pod,pvc,service -o wide
    "${kube[@]}" get configmap deployed -o 'jsonpath=deployed: {.data.commit}, at {.data.at}{"\n"}before it: {.data.previous}{"\n"}the installation'"'"'s definition: {.data.installation}; before it: {.data.previous-installation}{"\n"}' 2>/dev/null || true
    if answers; then echo "the service answers at $url"; else echo "the service does not answer at $url from this machine"; fi
    [ ! -s "$model_accounts_file" ] || echo "the model's account at the proxy: $(model_account_at_proxy || true)"
    ;;
env)
    mkdir -p .local
    umask 077
    printf 'export CMEM_URL=%s\nexport CMEM_TOKEN=%s\n' "$url" "$(secret laptop)" > .local/cmem.env
    printf 'export CMEM_URL=%s\nexport CMEM_DREAMER_TOKEN=%s\n' "$url" "$(secret dreamer)" > .local/cmem-dreamer.env
    echo "wrote .local/cmem.env (the laptop role) and .local/cmem-dreamer.env (the consolidation role)"
    # The owner's machine's sessions, for the dispatcher's hooks (ADR 0049).
    host_token="$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.data.host}' 2>/dev/null | base64 -d || true)"
    place="$("${kube[@]}" get deployment ${pre}dispatcher -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="DISPATCHER_PLACE")].value}' 2>/dev/null || true)"
    if [ -n "$host_token" ] && [ -n "$place" ]; then
        printf 'export DISPATCHER_URL=%s\nexport DISPATCHER_TOKEN=%s\nexport DISPATCHER_PLACE=%s\n' \
            "${DISPATCHER_URL:-http://127.0.0.1:8379}" "$host_token" "$place" > .local/dispatcher.env
        echo "wrote .local/dispatcher.env (the dispatcher's host role, place $place)"
    else
        echo "the dispatcher has no host token or names no place yet: deploy first; .local/dispatcher.env not written" >&2
    fi
    ;;
definition)
    case "${3:-}" in
    put)
        # The bundle a deploy puts in the cluster, without deploying.
        put_definition || die "no copy of the definition was put in the cluster"
        ;;
    apply)
        [ -n "${4:-}" ] || die "say which message holds the proposal: scripts/deploy.sh $context definition apply MESSAGE-ID"
        apply_definition "$4" "${5:-}"
        ;;
    *) die "say: definition apply MESSAGE-ID, definition apply --file FILE, or definition put" ;;
    esac
    ;;
secrets)
    # What a deploy does to the secrets, and nothing else: for a trial
    # installation (scripts/trial.sh), which applies the chart itself.
    # The dispatcher reads them when it starts: what applies the chart after
    # this restarts it, as a deploy does.
    make_secrets
    move_owner_token
    [ "${dispatcher_restart:-0}" = 0 ] || echo "the dispatcher's secret changed; it needs a restart, after the chart is applied"
    ;;
check)
    "${kube[@]}" exec ${pre}cmem-0 -- cmem check --library /data/library
    "${kube[@]}" exec ${pre}cmem-0 -- git -C /data/library fsck --strict --no-dangling >/dev/null && echo "git finds the repository whole"
    echo "notes: $("${kube[@]}" exec ${pre}cmem-0 -- sh -c 'find /data/library/entries -name "*.md" | wc -l') entries; last write: $("${kube[@]}" exec ${pre}cmem-0 -- git -C /data/library log -1 --format='%cd  %s' --date=iso)"
    ;;
backup)
    file="${3:-}"
    [ -n "$file" ] || die "name the file to write, for example cmem-library-$(date +%Y%m%d).tar"
    [ ! -e "$file" ] || die "$file already exists"
    "${kube[@]}" exec ${pre}cmem-0 -- tar -C /data -cf - library > "$file" || { rm -f "$file"; die "the copy failed"; }
    # Asked for by name: piping the listing into grep -q would stop tar early
    # on a large archive, and that reads as a failure.
    tar -tf "$file" library/.git/HEAD >/dev/null 2>&1 || { rm -f "$file"; die "what came back is not a library"; }
    echo "wrote $file: $(tar -tf "$file" | grep -c '^library/entries/.*\.md$' || true) entries, with their history"
    ;;
copies)
    from_store list
    ;;
copy)
    [ -n "${3:-}" ] || die "name what to get a copy of, for example: scripts/deploy.sh $context copy library > library.bundle"
    [ ! -t 1 ] || die "this writes the copy to standard output; send it to a file"
    from_store dump "$3" ${4:+"$4"}
    ;;
restore)
    file="${3:-}"
    fetched=""
    if [ "$file" = --from-store ]; then
        # The copy passes through this machine, in a file nobody else can read, and is removed after.
        fetched="$(mktemp)"
        from_store dump library ${4:+"$4"} > "$fetched" || { rm -f "$fetched"; die "no copy came from the store"; }
        file="$fetched"
    fi
    [ -f "$file" ] || die "name the backup file to restore, or --from-store"
    if head -c 16 "$file" | grep -q '^# v[0-9] git bundle'; then
        # One file made by `cmem backup`, which is what the nightly job keeps:
        # the memory service makes a library of it, beside the one in place.
        how='cat > /data/restoring.bundle && cmem restore /data/restoring.bundle /data/restoring/library >/dev/null && rm -f /data/restoring.bundle'
    elif tar -tf "$file" library/.git/HEAD >/dev/null 2>&1; then
        how='tar -C /data/restoring -xf -'
    else
        [ -z "$fetched" ] || rm -f "$fetched"
        die "$file is not a backup: neither a file made by cmem backup nor one made by this script"
    fi
    # The library in place is moved aside, never deleted.
    "${kube[@]}" exec -i ${pre}cmem-0 -- sh -c '
        set -e
        rm -rf /data/restoring /data/restoring.bundle && mkdir /data/restoring
        '"$how"'
        test -f /data/restoring/library/.git/HEAD
        aside=/data/library.replaced-$(date +%Y%m%dT%H%M%S)
        mv /data/library "$aside"
        mv /data/restoring/library /data/library
        rmdir /data/restoring
        echo "the library that was in place is kept as $aside in the volume"' < "$file"
    [ -z "$fetched" ] || rm -f "$fetched"
    # The service holds the old library in memory; restarting makes it read the restored one.
    "${kube[@]}" delete pod ${pre}cmem-0 --wait=true >/dev/null
    "${kube[@]}" rollout status statefulset/${pre}cmem --timeout=120s >/dev/null
    sleep 3
    echo "restored: $("${kube[@]}" exec ${pre}cmem-0 -- sh -c 'find /data/library/entries -name "*.md" | wc -l') entries"
    if answers; then echo "the service answers at $url"; else echo "the service does not answer at $url from this machine"; fi
    ;;
token)
    role="${3:-}"
    case "$role" in laptop | worker | dreamer) ;; *) die "name the role: laptop, worker or dreamer" ;; esac
    [ ! -t 1 ] || die "this prints a credential; pipe it to the program that stores it"
    secret "$role"
    ;;
secret)
    name="${3:-}" key="${4:-token}"
    [ -n "$name" ] || die "name the secret, for example: ... | scripts/deploy.sh $context secret model-credential"
    [ ! -t 0 ] || die "the value comes on standard input, from the program that holds it; it is never typed or given as an argument"
    # From standard input to the cluster's API, and nowhere else. The key is
    # merged into the secret, so that a secret can hold several.
    "${kube[@]}" get secret "$name" >/dev/null 2>&1 || "${kube[@]}" create secret generic "$name" >/dev/null
    "${kube[@]}" create secret generic "$name" "--from-file=$key=/dev/stdin" --dry-run=client -o 'jsonpath={.data}' | only_data |
        "${kube[@]}" patch secret "$name" --type merge --patch-file /dev/stdin >/dev/null
    [ "$("${kube[@]}" get secret "$name" -o "jsonpath={.data.$key}" | wc -c)" -ge 4 ] || die "the secret $name came out empty"
    echo "the cluster holds secret $name (key $key)"
    # The egress proxy reads its credentials when it starts.
    if [ "$name" = egress-credentials ] && "${kube[@]}" get deployment egress >/dev/null 2>&1; then
        "${kube[@]}" rollout restart deployment/egress >/dev/null && echo "restarted the egress proxy to read it"
    fi
    ;;
model-account)
    # An installation may have more than one account for the model. Their
    # credentials stay on this machine, each printed by a command of its
    # own; the proxy holds one. The choice is kept in the installation's
    # definition, and each change is in the dispatcher's log, since a usage
    # reading taken on one account says nothing of another.
    want="${3:-}"
    model_accounts
    at_proxy="$(model_account_at_proxy)"
    chosen="$(cat "$model_account_file" 2>/dev/null || true)"
    if [ -z "$want" ]; then
        echo "at the proxy: ${at_proxy:-the credential of no account listed in $model_accounts_file}; chosen in the installation's definition: ${chosen:-none}"
        awk '$1 !~ /^#/ && NF {print "  " $1}' "$model_accounts_file"
        exit 0
    fi
    command="$(model_account_command "$want")"
    [ -n "$command" ] || die "no account named $want in $model_accounts_file; it lists: $(awk '$1 !~ /^#/ && NF {printf "%s ", $1}' "$model_accounts_file")"
    restart="" now=""
    for flag in "${@:4}"; do
        case "$flag" in --restart) restart=1 ;; --now) now=1 ;; *) die "model-account NAME takes --restart and --now" ;; esac
    done
    # The meter reads the mounted secret again when the cluster changes it,
    # and logs each change with a count; a new pod reads it as it starts.
    meter_changes() { { "${kube[@]}" logs "deployment/${pre}egress" -c meter 2>/dev/null || true; } | sed -n 's/.*the model.s credential changed.*changes=\([0-9][0-9]*\).*/\1/p' | tail -1; }
    meter_pod() { "${kube[@]}" get pods -l "app=egress${pre:+,trial=${pre%-}}" --field-selector=status.phase=Running -o 'jsonpath={.items[*].metadata.name}' 2>/dev/null | cut -d' ' -f1; }
    restart_proxy() {
        if [ -z "$now" ]; then
            busy="$("${kube[@]}" get pods -l app.kubernetes.io/component=agent --field-selector=status.phase=Running -o 'jsonpath={range .items[?(@.metadata.ownerReferences[0].kind=="Job")]}{.metadata.name}{" "}{end}')"
            [ -z "$busy" ] || die "these jobs are running and the proxy's restart would cut their answers: $busy. Wait, or add --now"
        fi
        "${kube[@]}" rollout restart "deployment/${pre}egress" >/dev/null
        "${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null || die "the egress proxy did not come back; see: kubectl --context $context -n $namespace get pods"
    }
    changes_before="$(meter_changes)" pod_before="$(meter_pod)"
    # From the command to the cluster's API, and nowhere else.
    sh -c "$command" | tr -d '\n' | "$0" "$context" secret "$model_secret" token >/dev/null || die "the credential of account $want did not reach the cluster; the proxy holds what it held"
    [ "$(model_account_at_proxy)" = "$want" ] || die "the secret does not hold account $want's credential after the change"
    if ! "${kube[@]}" get "deployment/${pre}egress" >/dev/null 2>&1; then
        echo "there is no egress proxy here (a trial's); the secret $model_secret holds the credential"
    elif [ -n "$restart" ]; then
        restart_proxy
    elif [ "$at_proxy" != "$want" ]; then
        # The cluster updates the mounted file some tens of seconds after the secret.
        wait_for="${CARMAN_METER_WAIT:-180}" waited=0
        until { pod="$(meter_pod)"; [ -n "$pod" ] && [ "$pod" != "$pod_before" ]; } || [ "$(meter_changes)" -gt "${changes_before:-0}" ] 2>/dev/null; do
            [ "$waited" -lt "$wait_for" ] || die "the secret holds account $want's credential, and the meter did not take it within ${wait_for}s, so nothing is recorded; to restart the proxy, which cuts answers in flight: scripts/deploy.sh $context model-account $want --restart"
            sleep 5; waited=$((waited + 5))
        done
        echo "the meter took the new credential after about ${waited}s, with no restart"
    fi
    printf '%s\n' "$want" > "$model_account_file"
    # In a subshell: the client dies where there is no dispatcher, and the record is best-effort.
    (dispatch_client emit model.account --subject "$want" --source deploy) >/dev/null 2>&1 ||
        echo "deploy: the change was not recorded in the dispatcher's log; its usage readings from before are of the other account" >&2
    echo "the proxy holds account $want's credential (it held ${at_proxy:-an unlisted one}); recorded in $model_account_file: commit it there"
    # A reading of every model on the new account, so that its standing is known at once.
    if "${kube[@]}" get cronjob agent-usage >/dev/null 2>&1; then
        job="usage-account-$(date -u +%H%M%S)"
        "${kube[@]}" create job "$job" --from=cronjob/agent-usage --dry-run=client -o json |
            "${kube[@]}" set env --local -f - USAGE_ALL=1 -o json | "${kube[@]}" create -f - >/dev/null &&
            "${kube[@]}" wait --for=condition=complete "job/$job" --timeout=180s >/dev/null 2>&1 &&
            "${kube[@]}" logs "job/$job" 2>/dev/null | grep '^recorded' || echo "deploy: no reading could be taken on account $want; see: scripts/deploy.sh $context dispatch status" >&2
        "${kube[@]}" delete job "$job" --wait=false >/dev/null 2>&1 || true
    fi
    ;;
agent-image)
    build_agent
    echo "$CARMAN_AGENT_IMAGE"
    ;;
routine)
    file="${3:-}"
    [ -f "$file" ] || die "name the routine's file, for example .local/routines/review.md"
    "${kube[@]}" get configmap routines >/dev/null 2>&1 || "${kube[@]}" create configmap routines >/dev/null
    "${kube[@]}" create configmap routines "--from-file=$(basename "$file")=$file" --dry-run=client -o 'jsonpath={.data}' | only_data |
        "${kube[@]}" patch configmap routines --type merge --patch-file /dev/stdin >/dev/null
    echo "the cluster holds routine $(basename "$file" .md)"
    ;;
signin)
    # A person's sign-in to a service that has no key to hand out, kept by a
    # renewal beside the proxy (deploy/agent/oauth-credential). The browser
    # comes back to a port of this machine, forwarded to the renewal.
    key="${3:-}" url="${4:-}" port="${5:-8765}"
    [ -n "$key" ] && [ -n "$url" ] || die "say: signin KEY URL, with the key of a renewal in the values and the service's address"
    "${kube[@]}" get deployment egress -o 'jsonpath={.spec.template.spec.containers[*].name}' | tr ' ' '\n' | grep -qx "renew-$key" ||
        die "no renewal with the key $key; one is an entry under egress.renewals in the values"
    "${kube[@]}" port-forward deployment/egress "$port:$port" >/dev/null 2>&1 &
    forward=$!
    trap 'kill "$forward" 2>/dev/null' EXIT
    "${kube[@]}" exec deployment/egress -c "renew-$key" -- oauth-credential signin "$key" "$url" --port "$port"
    ;;
tool)
    file="${3:-}"
    [ -f "$file" ] || die "name the command's file, for example .local/tools/NAME"
    "${kube[@]}" get configmap tools >/dev/null 2>&1 || "${kube[@]}" create configmap tools >/dev/null
    "${kube[@]}" create configmap tools "--from-file=$(basename "$file")=$file" --dry-run=client -o 'jsonpath={.data}' | only_data |
        "${kube[@]}" patch configmap tools --type merge --patch-file /dev/stdin >/dev/null
    echo "agents have the command $(basename "$file")"
    ;;
agent)
    if [ "${3:-}" = resume ]; then
        resume "${4:-}"
        exit 0
    fi
    name="${4:-}"
    [ -n "$name" ] || die "say: agent run NAME, agent log NAME, agent attach NAME or agent resume [ID]"
    if [ "${3:-}" = restart ]; then
        "${kube[@]}" get statefulset "agent-$name" >/dev/null 2>&1 || die "no interactive agent named $name"
        "${kube[@]}" delete pod "agent-$name-0" --wait=true >/dev/null
        "${kube[@]}" rollout status "statefulset/agent-$name" --timeout=300s >/dev/null 2>&1 || true
        "${kube[@]}" wait --for=condition=ready "pod/agent-$name-0" --timeout=300s >/dev/null
        echo "restarted agent $name, on $("${kube[@]}" get pod "agent-$name-0" -o 'jsonpath={.spec.containers[0].image}')"
        exit 0
    fi
    if [ "${3:-}" = attach ]; then
        "${kube[@]}" get statefulset "agent-$name" >/dev/null 2>&1 || die "no interactive agent named $name; one is an entry under 'agents' in the values with interactive: true"
        attach "agent-$name-0"
    fi
    "${kube[@]}" get cronjob "agent-$name" >/dev/null 2>&1 || die "no agent named $name that runs as a job; agents are listed under 'agents' in the values"
    case "${3:-}" in
    run)
        job="agent-$name-$(date -u +%Y%m%d-%H%M%S)"
        "${kube[@]}" create job "$job" "--from=cronjob/agent-$name" >/dev/null
        echo "started $job; waiting for it to finish (up to an hour)"
        finished "$job" 3600 || echo "the run did not complete; what it printed follows" >&2
        "${kube[@]}" logs "job/$job" --tail=-1
        ;;
    log)
        job="$("${kube[@]}" get jobs -l "app=agent-$name" --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -1)"
        [ -n "$job" ] || job="$("${kube[@]}" get jobs --sort-by=.metadata.creationTimestamp -o name | grep "/agent-$name-" | tail -1)"
        [ -n "$job" ] || die "$name has not run yet"
        "${kube[@]}" logs "$job" --tail=-1
        ;;
    *) die "say: agent run NAME, or agent log NAME" ;;
    esac
    ;;
dreamer)
    case "${3:-}" in
    on | off)
        "${kube[@]}" get secret model-credential >/dev/null 2>&1 || [ "$3" = off ] ||
            die "the dreamer needs a model credential first: ... | scripts/deploy.sh $context secret model-credential"
        if [ -n "$("${kube[@]}" get cronjob dreamer -o 'jsonpath={.metadata.annotations.dispatcher/scheduled}' 2>/dev/null)" ]; then
            # The dispatcher starts it: its CronJob stays suspended, and the
            # dispatcher reads its schedules when it starts (ADR 0059).
            "${kube[@]}" annotate --overwrite cronjob dreamer "dispatcher/scheduled=$3" >/dev/null
            echo "the dreamer is $3 from the next deploy (scripts/deploy.sh $context), which gives the dispatcher its schedule: $("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.schedule}') (UTC)"
        else
            "${kube[@]}" patch cronjob dreamer --type merge -p "{\"spec\":{\"suspend\":$([ "$3" = off ] && echo true || echo false)}}" >/dev/null
            echo "the dreamer's schedule is $3: $("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.schedule}') (UTC)"
        fi
        ;;
    run)
        job="dreamer-$(date -u +%Y%m%d-%H%M%S)"
        "${kube[@]}" create job "$job" --from=cronjob/dreamer >/dev/null
        echo "started $job; waiting for it to finish (up to 30 minutes)"
        finished "$job" 1800 || echo "the run did not complete; what it printed follows" >&2
        "${kube[@]}" logs "job/$job" --tail=-1
        ;;
    log)
        job="$("${kube[@]}" get jobs -l app=dreamer --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -1)"
        [ -n "$job" ] || job="$("${kube[@]}" get jobs --sort-by=.metadata.creationTimestamp -o name | grep '/dreamer' | tail -1)"
        [ -n "$job" ] || die "the dreamer has not run yet"
        "${kube[@]}" logs "$job" --tail=-1
        ;;
    *) die "say: dreamer on, dreamer off, dreamer run or dreamer log" ;;
    esac
    ;;
dispatch)
    case "${3:-}" in
    status | log | submit | emit | deployed | peers | send | reply | inbox | read | message | watch | improve | lift | limit)
        # Messages (ADR 0045) are sent and read as the owner; and the list
        # of improvements to the platform (ADR 0058).
        dispatch_client "${@:3}"
        ;;
    dump)
        [ ! -t 1 ] || die "this writes the dump to standard output; send it to a file"
        dispatch_client dump
        ;;
    restore)
        file="${4:-}"
        [ -f "$file" ] || die "name the dump to restore, a file made by: dispatch dump > FILE"
        grep -q '^-- PostgreSQL database dump complete' "$file" || die "$file is not a whole dump: its last line is missing"
        "${kube[@]}" get statefulset ${pre}dispatcher-db >/dev/null 2>&1 || die "the dispatcher's database is the installation's own here; restore it there"
        # The service stops, so that nothing writes while the database is
        # replaced; the database in place is renamed, never dropped.
        "${kube[@]}" scale deployment/${pre}dispatcher --replicas=0 >/dev/null
        "${kube[@]}" wait --for=delete pod -l app=dispatcher --timeout=120s >/dev/null 2>&1 || true
        aside="dispatcher_replaced_$(date +%Y%m%d%H%M%S)"
        status=0
        "${kube[@]}" exec -i ${pre}dispatcher-db-0 -- sh -c '
            set -e
            db="$POSTGRES_DB"
            psql -q -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres -c "ALTER DATABASE \"$db\" RENAME TO '"$aside"'"
            createdb -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$db"
            psql -q -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$db" >/dev/null
            echo "the database that was in place is kept as '"$aside"'"' < "$file" || status=1
        "${kube[@]}" scale deployment/${pre}dispatcher --replicas=1 >/dev/null
        "${kube[@]}" rollout status deployment/${pre}dispatcher --timeout=180s >/dev/null
        [ "$status" -eq 0 ] || die "the restore failed; the service runs on what is in place now"
        sleep 3
        dispatch_client status
        ;;
    owner-token)
        "${kube[@]}" get secret ${pre}dispatcher-tokens >/dev/null 2>&1 || die "the cluster has no dispatcher token secret yet: deploy first"
        case "${4:-}" in
        new)
            # Whoever may deploy may do this, as they could always read the token.
            ! cluster_has_owner_token || die "the cluster still holds an owner token from before ADR 0051: deploy, which moves it"
            owner_sha256="$(new_owner_token)"
            "${kube[@]}" patch secret ${pre}dispatcher-tokens --type merge --patch-file /dev/stdin >/dev/null <<EOF
{"stringData": {"owner-sha256": "$owner_sha256"}}
EOF
            "${kube[@]}" rollout restart deployment/${pre}dispatcher >/dev/null
            "${kube[@]}" rollout status deployment/${pre}dispatcher --timeout=180s >/dev/null
            echo "made a new owner token in $owner_token_file; the dispatcher knows its hash, and the token before it no longer works"
            ;;
        back)
            # A build from before ADR 0051 takes the owner's token in the
            # clear: the key owner, and in DISPATCHER_TOKENS. The current
            # build refuses to start on that, so the older commit is
            # deployed straight after (deploy/RECOVERY.md).
            [ -s "$owner_token_file" ] || die "no owner token at $owner_token_file to put back"
            ! cluster_has_owner_token || die "the cluster holds an owner token already"
            owner="$(tr -d '\n' < "$owner_token_file")"
            tokens="$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.data.DISPATCHER_TOKENS}' | base64 -d)"
            "${kube[@]}" patch secret ${pre}dispatcher-tokens --type merge --patch-file /dev/stdin >/dev/null <<EOF
{"stringData": {"owner": "$owner", "DISPATCHER_TOKENS": "owner=$owner,$tokens"}}
EOF
            echo "the owner's token is on the cluster again, for a build from before ADR 0051: deploy that commit now. A later deploy of this one makes a new token."
            ;;
        *) die "say: dispatch owner-token new, or dispatch owner-token back" ;;
        esac
        ;;
    *) die "say: dispatch status [ID], dispatch log, dispatch submit CLASS [ID], dispatch emit TYPE, dispatch dump > FILE, dispatch restore FILE, dispatch owner-token new|back, or for messages: dispatch peers, send TO 'TEXT', reply ID 'TEXT', inbox [--read], read ID, message ID, watch SESSION; for the list of improvements: dispatch improve [list|show ID|agree ID|decline ID 'WHY'|...]" ;;
    esac
    ;;
*)
    die "unknown action $action; say deploy, status, secrets, check, backup, restore, env, token, secret, agent-image, dreamer, routine, tool, agent or dispatch"
    ;;
esac
