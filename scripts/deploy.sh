#!/usr/bin/env bash
# Deploys the memory service to a Kubernetes cluster, named by its kubectl
# context. The context is always given; this never acts on the default one.
#
#   scripts/deploy.sh CONTEXT           build the image, apply, wait until it serves
#   scripts/deploy.sh CONTEXT status    what is running, and whether it answers
#   scripts/deploy.sh CONTEXT env       write the laptop's settings to .local/cmem.env
#                                       and the consolidation role's to .local/cmem-dreamer.env
#   scripts/deploy.sh CONTEXT check     ask the service's library whether every note is as written
#   scripts/deploy.sh CONTEXT backup FILE    copy the whole library, history included, to a tar file
#   scripts/deploy.sh CONTEXT restore FILE   put a backup in place of the library and restart
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
#
# What is applied is an overlay under deploy/k8s; `local` unless CARMAN_OVERLAY
# names another. CMEM_URL is where this machine reaches the service, and is
# http://127.0.0.1:8377 unless set, which is right for the local overlay.
#
# The image is built with `docker build`. On the local cluster (context devvm)
# that builds straight into the cluster's container runtime, so no registry is
# needed. For a cluster that pulls from a registry, push the image yourself
# and set CARMAN_IMAGE to its name.
#
# Tokens are made once, on the first deploy, and live only in the cluster's
# secret and in the files `env` writes. They are never printed.
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
kube=(kubectl --context "$context" --namespace carman)
# CARMAN_KUBE_SERVER overrides the API address, for reaching it through a tunnel.
[ -z "${CARMAN_KUBE_SERVER:-}" ] || kube+=(--server "$CARMAN_KUBE_SERVER")
overlay="deploy/k8s/${CARMAN_OVERLAY:-local}"
[ -d "$overlay" ] || die "no overlay at $overlay"
url="${CMEM_URL:-http://127.0.0.1:8377}"

answers() { curl -fsS -m 3 -o /dev/null "$url/healthz" 2>/dev/null; }

secret() { "${kube[@]}" get secret cmem-tokens -o "jsonpath={.data.$1}" | base64 -d; }

# The build context has to be somewhere the container runtime can see.
build_root="${CARMAN_BENCH_ROOT:-$HOME/carman-bench}/build"
linux_cmem() {
    local arch build=(go build)
    arch="$(docker info --format '{{.Architecture}}')"
    case "$arch" in aarch64 | arm64) arch=arm64 ;; x86_64 | amd64) arch=amd64 ;; *) die "unknown architecture $arch" ;; esac
    command -v go >/dev/null || build=(nix develop -c go build)
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "${build[@]}" -o "$1" ./cmd/cmem
}

# The agent image: the harness, the memory client and the routines. Sets
# CARMAN_AGENT_IMAGE unless it is already set to an image pushed elsewhere.
build_agent() {
    [ -z "${CARMAN_AGENT_IMAGE:-}" ] || return 0
    local root="$build_root/agent"
    rm -rf "$root" && mkdir -p "$root"
    linux_cmem "$root/cmem"
    cp deploy/agent/Dockerfile deploy/agent/run-routine "$root/"
    cp -R routines "$root/routines"
    # A new tag whenever anything in it changes.
    CARMAN_AGENT_IMAGE="carman-agent:$(cd "$root" && find . -type f | sort | xargs shasum -a 256 | shasum -a 256 | cut -c1-12)"
    docker image inspect "$CARMAN_AGENT_IMAGE" >/dev/null 2>&1 || docker build -q -t "$CARMAN_AGENT_IMAGE" "$root" >/dev/null
    echo "built $CARMAN_AGENT_IMAGE" >&2
}

case "$action" in
deploy)
    if [ -z "${CARMAN_IMAGE:-}" ]; then
        root="$build_root/service"
        mkdir -p "$root"
        linux_cmem "$root/cmem"
        cp deploy/Dockerfile "$root/Dockerfile"
        # A new tag whenever the binary changes, so the cluster rolls it out.
        CARMAN_IMAGE="carman-cmem:$(shasum -a 256 "$root/cmem" | cut -c1-12)"
        docker build -q -t "$CARMAN_IMAGE" "$root" >/dev/null
        echo "built $CARMAN_IMAGE"
    fi

    "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: carman
EOF
    if ! "${kube[@]}" get secret cmem-tokens >/dev/null 2>&1; then
        laptop="$(openssl rand -hex 24)" worker="$(openssl rand -hex 24)" dreamer="$(openssl rand -hex 24)"
        # Through standard input, so the tokens are never on a command line.
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: cmem-tokens
  namespace: carman
stringData:
  CMEM_TOKENS: "laptop=$laptop,worker=$worker,dreamer=$dreamer"
  laptop: "$laptop"
  worker: "$worker"
  dreamer: "$dreamer"
EOF
        echo "made the token secret"
    fi

    build_agent
    # Whether the dreamer's schedule is on is a setting of the cluster, not of
    # the manifest: a deploy leaves it as it was, and off the first time.
    dreaming="$("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.suspend}' 2>/dev/null || true)"
    kubectl kustomize "$overlay" |
        sed -e "s|CMEM_IMAGE|$CARMAN_IMAGE|" -e "s|AGENT_IMAGE|$CARMAN_AGENT_IMAGE|" -e "s|suspend: true|suspend: ${dreaming:-true}|" |
        "${kube[@]}" apply -f -
    "${kube[@]}" rollout status statefulset/cmem --timeout=120s
    sleep 3 # a port forward to this machine, if there is one, takes a moment
    if answers; then
        echo "the service answers at $url"
    else
        echo "the service is running in the cluster but does not answer at $url from this machine;"
        echo "see deploy/README.md, \"Reaching it from the laptop\""
    fi
    # Every deploy that changes a binary makes a new image tag. Where images
    # are built straight into the cluster's runtime, the old ones only fill
    # its disk: remove those this deploy did not use. One that a finished Job
    # still names is left by the runtime, and that is fine.
    for old in $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E '^carman-(cmem|agent):' || true); do
        [ "$old" = "$CARMAN_IMAGE" ] || [ "$old" = "$CARMAN_AGENT_IMAGE" ] || docker rmi "$old" >/dev/null 2>&1 || true
    done
    ;;
status)
    "${kube[@]}" get statefulset,pod,pvc,service -o wide
    if answers; then echo "the service answers at $url"; else echo "the service does not answer at $url from this machine"; fi
    ;;
env)
    mkdir -p .local
    umask 077
    printf 'export CMEM_URL=%s\nexport CMEM_TOKEN=%s\n' "$url" "$(secret laptop)" > .local/cmem.env
    printf 'export CMEM_URL=%s\nexport CMEM_DREAMER_TOKEN=%s\n' "$url" "$(secret dreamer)" > .local/cmem-dreamer.env
    echo "wrote .local/cmem.env (the laptop role) and .local/cmem-dreamer.env (the consolidation role)"
    ;;
check)
    "${kube[@]}" exec cmem-0 -- cmem check --library /data/library
    "${kube[@]}" exec cmem-0 -- git -C /data/library fsck --strict --no-dangling >/dev/null && echo "git finds the repository whole"
    echo "notes: $("${kube[@]}" exec cmem-0 -- sh -c 'find /data/library/entries -name "*.md" | wc -l') entries; last write: $("${kube[@]}" exec cmem-0 -- git -C /data/library log -1 --format='%cd  %s' --date=iso)"
    ;;
backup)
    file="${3:-}"
    [ -n "$file" ] || die "name the file to write, for example cmem-library-$(date +%Y%m%d).tar"
    [ ! -e "$file" ] || die "$file already exists"
    "${kube[@]}" exec cmem-0 -- tar -C /data -cf - library > "$file" || { rm -f "$file"; die "the copy failed"; }
    # Asked for by name: piping the listing into grep -q would stop tar early
    # on a large archive, and that reads as a failure.
    tar -tf "$file" library/.git/HEAD >/dev/null 2>&1 || { rm -f "$file"; die "what came back is not a library"; }
    echo "wrote $file: $(tar -tf "$file" | grep -c '^library/entries/.*\.md$' || true) entries, with their history"
    ;;
restore)
    file="${3:-}"
    [ -f "$file" ] || die "name the backup file to restore"
    tar -tf "$file" library/.git/HEAD >/dev/null 2>&1 || die "$file is not a backup made by this script"
    # The library in place is moved aside, never deleted.
    "${kube[@]}" exec -i cmem-0 -- sh -c '
        set -e
        rm -rf /data/restoring && mkdir /data/restoring
        tar -C /data/restoring -xf -
        test -f /data/restoring/library/.git/HEAD
        aside=/data/library.replaced-$(date +%Y%m%dT%H%M%S)
        mv /data/library "$aside"
        mv /data/restoring/library /data/library
        rmdir /data/restoring
        echo "the library that was in place is kept as $aside in the volume"' < "$file"
    # The service holds the old library in memory; restarting makes it read the restored one.
    "${kube[@]}" delete pod cmem-0 --wait=true >/dev/null
    "${kube[@]}" rollout status statefulset/cmem --timeout=120s >/dev/null
    sleep 3
    echo "restored $file: $("${kube[@]}" exec cmem-0 -- sh -c 'find /data/library/entries -name "*.md" | wc -l') entries"
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
    [ -n "$name" ] || die "name the secret, for example: ... | scripts/deploy.sh $context secret carman-model"
    [ ! -t 0 ] || die "the value comes on standard input, from the program that holds it; it is never typed or given as an argument"
    # From standard input to the cluster's API, and nowhere else.
    "${kube[@]}" create secret generic "$name" "--from-file=$key=/dev/stdin" --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
    [ "$("${kube[@]}" get secret "$name" -o "jsonpath={.data.$key}" | wc -c)" -gt 8 ] || die "the secret $name came out empty"
    echo "the cluster holds secret $name (key $key)"
    ;;
agent-image)
    build_agent
    echo "$CARMAN_AGENT_IMAGE"
    ;;
dreamer)
    case "${3:-}" in
    on | off)
        "${kube[@]}" get secret carman-model >/dev/null 2>&1 || [ "$3" = off ] ||
            die "the dreamer needs a model credential first: ... | scripts/deploy.sh $context secret carman-model"
        "${kube[@]}" patch cronjob dreamer --type merge -p "{\"spec\":{\"suspend\":$([ "$3" = off ] && echo true || echo false)}}" >/dev/null
        echo "the dreamer's schedule is $3: $("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.schedule}') (UTC)"
        ;;
    run)
        job="dreamer-$(date -u +%Y%m%d-%H%M%S)"
        "${kube[@]}" create job "$job" --from=cronjob/dreamer >/dev/null
        echo "started $job; waiting for it to finish (up to 30 minutes)"
        "${kube[@]}" wait --for=condition=complete --timeout=1800s "job/$job" >/dev/null 2>&1 ||
            echo "the run did not complete; what it printed follows" >&2
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
*)
    die "unknown action $action; say deploy, status, check, backup, restore, env, token, secret, agent-image or dreamer"
    ;;
esac
