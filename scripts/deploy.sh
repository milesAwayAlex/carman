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
#   scripts/deploy.sh CONTEXT restore --from-store [COPY]   the same, with the latest copy the nightly job kept (or the one with that id)
#   scripts/deploy.sh CONTEXT copies         the copies the nightly job has kept
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
#   scripts/deploy.sh CONTEXT routine FILE      put a routine of this installation's own in the cluster
#   scripts/deploy.sh CONTEXT tool FILE         put a command of this installation's own on agents' PATH
#   scripts/deploy.sh CONTEXT signin KEY URL    sign in, in a browser, to a service a renewal keeps a sign-in for
#   scripts/deploy.sh CONTEXT agent run NAME    run an agent once now and show what it reported
#   scripts/deploy.sh CONTEXT agent log NAME    the report of its latest run
#   scripts/deploy.sh CONTEXT agent attach NAME attach this terminal to an interactive agent's session
#   scripts/deploy.sh CONTEXT agent restart NAME restart an interactive agent, onto the image last deployed;
#                                       whatever runs in it ends, and conversations resume from disk
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
# The namespace everything lives in; CARMAN_NAMESPACE names another.
namespace="${CARMAN_NAMESPACE:-carman}"
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

secret() { "${kube[@]}" get secret cmem-tokens -o "jsonpath={.data.$1}" | base64 -d; }

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
    linux_cmem "$root"
    cp deploy/agent/Dockerfile deploy/agent/run-routine deploy/agent/backup-state deploy/agent/agent-env deploy/agent/agent-session deploy/agent/agent-attach deploy/agent/agent-resume deploy/agent/agent-open deploy/agent/oauth-credential deploy/agent/attention.md deploy/agent/renew-credential deploy/agent/settings.json "$root/"
    cp -R routines "$root/routines"
    # A new tag whenever anything in it changes.
    # CARMAN_AGENT_BASE names an image to build on in place of the plain one:
    # an installation's own shell and tools (deploy/agent/Dockerfile).
    local base=()
    [ -z "${CARMAN_AGENT_BASE:-}" ] || base=(--build-arg "BASE=$CARMAN_AGENT_BASE")
    CARMAN_AGENT_IMAGE="${registry}carman-agent:$( (cd "$root" && find . -type f | sort | xargs shasum -a 256; echo "${CARMAN_AGENT_BASE:-}") | shasum -a 256 | cut -c1-12)"
    docker image inspect "$CARMAN_AGENT_IMAGE" >/dev/null 2>&1 || docker build -q --label built-by=deploy "${base[@]}" -t "$CARMAN_AGENT_IMAGE" "$root" >/dev/null || {
        echo "deploy: the agent image did not build; the build again, with its output:" >&2
        docker build --label built-by=deploy "${base[@]}" -t "$CARMAN_AGENT_IMAGE" "$root" >&2 || true
        exit 1
    }
    publish "$CARMAN_AGENT_IMAGE"
    echo "built $CARMAN_AGENT_IMAGE" >&2
}

# Wraps an object's data, as kubectl prints it, into a patch that changes
# only the data.
only_data() { printf '{"data":'; cat; printf '}'; }

case "$action" in
deploy)
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
    if ! "${kube[@]}" get secret cmem-tokens >/dev/null 2>&1; then
        laptop="$(openssl rand -hex 24)" worker="$(openssl rand -hex 24)" dreamer="$(openssl rand -hex 24)"
        # Through standard input, so the tokens are never on a command line.
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: cmem-tokens
  namespace: $namespace
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
    # What the egress proxy is to hold a credential for, from the values.
    egress="$(helm template carman charts/carman --namespace "$namespace" "${chart_values[@]}" --set memory.image=m:0 --set agent.image=a:0 --show-only templates/egress.yaml 2>/dev/null || true)"
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
    helm template carman charts/carman --namespace "$namespace" "${chart_values[@]}" \
        --set "memory.image=$CARMAN_IMAGE" --set "agent.image=$CARMAN_AGENT_IMAGE" --set "egress.renewalImage=$renewal_image" --set "dreamer.suspend=${dreaming:-true}" |
        kubectl --context "$context" apply -f -   # every object names its namespace; one is not this one
    "${kube[@]}" rollout status statefulset/cmem --timeout=120s
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
    # Every deploy that changes a binary makes a new image tag. Where images
    # are built straight into the cluster's runtime, the old ones only fill
    # its disk: remove those this deploy did not use and no pod names (an
    # interactive agent keeps its image until it is restarted). One that a
    # finished Job still names is left by the runtime, and that is fine.
    in_use=" $("${kube[@]}" get pods -o 'jsonpath={.items[*].spec.containers[*].image} {.items[*].spec.initContainers[*].image}' 2>/dev/null) "
    for old in $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E '^([^/]+/)?carman-(cmem|agent):' || true); do
        [ "$old" = "$CARMAN_IMAGE" ] || [ "$old" = "$CARMAN_AGENT_IMAGE" ] || [ "${in_use#* $old }" != "$in_use" ] || docker rmi "$old" >/dev/null 2>&1 || true
    done
    # The cluster's runtime keeps a record of its own for an image it has
    # run, which outlives the tag: remove those of this script's builds too.
    # Only what carries this script's label, and never by force: what a
    # container still uses is refused, and other images are not looked at.
    for old in $(docker images --filter dangling=true -q 2>/dev/null | sort -u); do
        [ "$(docker image inspect "$old" --format '{{index .Config.Labels "built-by"}}' 2>/dev/null | tail -1)" = deploy ] || continue
        docker rmi "$old" >/dev/null 2>&1 || true
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
copies)
    from_store list
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
    "${kube[@]}" exec -i cmem-0 -- sh -c '
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
    "${kube[@]}" delete pod cmem-0 --wait=true >/dev/null
    "${kube[@]}" rollout status statefulset/cmem --timeout=120s >/dev/null
    sleep 3
    echo "restored: $("${kube[@]}" exec cmem-0 -- sh -c 'find /data/library/entries -name "*.md" | wc -l') entries"
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
    [ "$("${kube[@]}" get secret "$name" -o "jsonpath={.data.$key}" | wc -c)" -gt 8 ] || die "the secret $name came out empty"
    echo "the cluster holds secret $name (key $key)"
    # The egress proxy reads its credentials when it starts.
    if [ "$name" = egress-credentials ] && "${kube[@]}" get deployment egress >/dev/null 2>&1; then
        "${kube[@]}" rollout restart deployment/egress >/dev/null && echo "restarted the egress proxy to read it"
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
    name="${4:-}"
    [ -n "$name" ] || die "say: agent run NAME, agent log NAME or agent attach NAME"
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
        [ -t 0 ] || die "attach needs a terminal"
        # Where this attach comes from: the tmux pane it runs in, or else the
        # terminal. A later attach from the same place replaces the client
        # this one leaves in the pod if its connection is dropped.
        from="$(hostname -s):$(tty 2>/dev/null | tr '/' '_')"
        [ -z "${TMUX:-}" ] || from="$(hostname -s):tmux:$(tmux display-message -p '#{session_name}:#{window_index}.#{pane_index}' 2>/dev/null)"
        exec "${kube[@]}" exec -it "agent-$name-0" -- env AGENT_TERM="${TERM:-}" COLORTERM="${COLORTERM:-truecolor}" AGENT_ATTACH_ID="$from" agent-attach
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
        "${kube[@]}" patch cronjob dreamer --type merge -p "{\"spec\":{\"suspend\":$([ "$3" = off ] && echo true || echo false)}}" >/dev/null
        echo "the dreamer's schedule is $3: $("${kube[@]}" get cronjob dreamer -o 'jsonpath={.spec.schedule}') (UTC)"
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
*)
    die "unknown action $action; say deploy, status, check, backup, restore, env, token, secret, agent-image, dreamer, routine, tool or agent"
    ;;
esac
