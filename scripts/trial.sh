#!/usr/bin/env bash
# A trial installation in a namespace where whoever runs this may run
# workloads but not make accounts, roles or network policies: an agent's
# sandbox (ADR 0037). The dispatcher with its own Postgres and the memory
# service on an empty library, made from this checkout, with tokens made
# here and no credential of a real installation's (ADR 0054).
#
#   scripts/trial.sh CONTEXT up [--old-secret]   build, make the secrets, apply, wait until it answers;
#                                        --old-secret makes the dispatcher's token secret as a
#                                        build from before ADR 0051 did, for trying the move
#   scripts/trial.sh CONTEXT check       whether the memory service and the dispatcher answer,
#                                        and a task of the probe class run through the dispatcher
#   scripts/trial.sh CONTEXT deploy ...  scripts/deploy.sh against the trial (its secrets action,
#                                        its dispatch commands), with the trial's own owner token
#   scripts/trial.sh CONTEXT down        delete everything the trial made, and only that
#
# The namespace is the context's own unless TRIAL_NAMESPACE names one.
# Everything made carries the label trial=TRIAL_NAME (trial unless set), and
# `down` deletes only what carries it. The chart's names are kept, so that a
# trial runs the same manifests and scripts as an installation: `up` refuses
# where an object of one of those names is there and is not the trial's.
#
# Images are built with BuildKit (buildctl, BUILDKIT_HOST) and pushed to
# TRIAL_REGISTRY (localhost:5050), as TRIAL_NAME-memory and
# TRIAL_NAME-dispatcher; the registry keeps them after `down`.
# The trial's owner token is in .local/trial/dispatcher-owner.token, with
# what the builds made; `down` removes that directory.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "trial: $*" >&2; exit 1; }

context="${1:-}" action="${2:-}"
[ -n "$context" ] && [ -n "$action" ] || die "say: scripts/trial.sh CONTEXT up|check|deploy|down"
name="${TRIAL_NAME:-trial}"
case "$name" in carman*) die "TRIAL_NAME may not start with the project's name" ;; esac
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
[ -n "$namespace" ] || die "the context $context names no namespace; set TRIAL_NAMESPACE"
kube=(kubectl --context "$context" --namespace "$namespace")
registry="${TRIAL_REGISTRY:-localhost:5050}"
label="trial=$name"
work=".local/trial"
# What the chart makes that a sandbox lets nobody but the installation make.
refused='ServiceAccount Role RoleBinding NetworkPolicy'
# The kinds a trial leaves, for `down`.
kinds=deployments,statefulsets,services,configmaps,secrets,cronjobs,jobs,pods,persistentvolumeclaims
# The trial's dispatcher runs as the account the installation makes for it
# where agent.sandbox.trial is on (ADR 0054), and reaches the cluster's API;
# without it, as the namespace's default, which can do neither, so the
# dispatcher answers but starts nothing. Every other pod runs as the default.
account=default
! "${kube[@]}" get serviceaccount trial-dispatcher >/dev/null 2>&1 || account=trial-dispatcher
# TRIAL_SOURCE: another checkout to build and render from (an older commit,
# for trying a move from it); its secrets are still made by this one's.
src="${TRIAL_SOURCE:-.}"

deploy_sh() {
    CARMAN_NAMESPACE="$namespace" CARMAN_OWNER_TOKEN_FILE="$work/dispatcher-owner.token" scripts/deploy.sh "$context" "$@"
}

# Builds an image of a binary of this checkout with its Dockerfile, and
# prints its name. Where the builder's RUN steps cannot fetch packages, the
# same image is made on a base that has them already (TRIAL_BASE_NAME).
build_image() {  # NAME CMD DOCKERFILE FALLBACK-BASE
    local dir="$work/build/$1" arch tag ref log
    rm -rf "$dir" && mkdir -p "$dir"
    arch="$(kubectl --context "$context" get --raw /version 2>/dev/null | sed -n 's/.*"platform": *"linux\/\([a-z0-9]*\)".*/\1/p')"
    [ -n "$arch" ] || arch="$(uname -m | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
    (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -o "$OLDPWD/$dir/$2-$arch" "./cmd/$2")
    cp "$3" "$dir/Dockerfile"
    tag="$(cat "$dir"/* | shasum -a 256 | cut -c1-12)"
    ref="$registry/$name-$1:$tag"
    log="$dir/build.log"
    if ! buildctl build --progress plain --frontend dockerfile.v0 --local context="$dir" --local dockerfile="$dir" \
        --opt platform="linux/$arch" --output "type=image,name=$ref,push=true" > "$log" 2>&1; then
        echo "trial: $1's Dockerfile did not build here ($(grep -m1 -E 'ERROR|WARNING: fetching' "$log" | cut -c1-120)); building it on $4" >&2
        sed -e "s|^FROM .*|FROM $4|" -e '/^RUN /d' "$3" > "$dir/Dockerfile"
        ref="$registry/$name-$1:$tag-base"
        buildctl build --progress plain --frontend dockerfile.v0 --local context="$dir" --local dockerfile="$dir" \
            --opt platform="linux/$arch" --output "type=image,name=$ref,push=true" > "$log" 2>&1 ||
            { tail -20 "$log" >&2; die "$1's image did not build"; }
    fi
    echo "$ref"
}

# The chart, rendered for the trial and made fit for the namespace: without
# what only the installation may make, every pod as the trial's account,
# every object and pod labelled, and each selector narrowed to the label.
render() {
    helm template "$name" "$src/charts/carman" --namespace "$namespace" -f deploy/values/trial.yaml \
        --set "memory.image=$memory_image" --set "dispatcher.image=$dispatcher_image" --set "agent.image=$dispatcher_image" |
        kubectl --context "$context" create --dry-run=client -o json -f - |
        TRIAL_REFUSED="$refused" TRIAL_LABEL="$name" TRIAL_ACCOUNT="$account" TRIAL_IMAGE="$dispatcher_image" python3 -c '
import json, os, sys
refused = os.environ["TRIAL_REFUSED"].split()
name, account = os.environ["TRIAL_LABEL"], os.environ["TRIAL_ACCOUNT"]
text, out, at = sys.stdin.read(), [], 0
dec = json.JSONDecoder()
while True:
    while at < len(text) and text[at].isspace():
        at += 1
    if at >= len(text):
        break
    obj, at = dec.raw_decode(text, at)
    out.append(obj)
# The class the trial runs through the dispatcher: a suspended CronJob whose
# job runs true, in the dispatcher image.
out.append({"apiVersion": "batch/v1", "kind": "CronJob", "metadata": {"name": "probe"},
    "spec": {"schedule": "0 0 1 1 *", "suspend": True, "jobTemplate": {"spec": {"backoffLimit": 0, "template": {"spec": {
        "restartPolicy": "Never", "containers": [{"name": "probe", "image": os.environ["TRIAL_IMAGE"], "command": ["/bin/true"]}]}}}}}})
kept = []
for o in out:
    if o["kind"] in refused:
        continue
    o["metadata"].setdefault("labels", {})["trial"] = name
    spec = o.get("spec", {})
    pod = spec.get("template") or spec.get("jobTemplate", {}).get("spec", {}).get("template")
    if pod is not None:
        pod.setdefault("metadata", {}).setdefault("labels", {})["trial"] = name
        pod["spec"]["serviceAccountName"] = "default"
        if o["kind"] == "Deployment" and o["metadata"]["name"] == "dispatcher" and account != "default":
            pod["spec"]["serviceAccountName"] = account
            pod["metadata"]["labels"]["reaches-the-api"] = "true"
    if o["kind"] in ("Deployment", "StatefulSet"):
        spec["selector"].setdefault("matchLabels", {})["trial"] = name
    if o["kind"] == "Service" and spec.get("selector"):
        spec["selector"]["trial"] = name
    for claim in spec.get("volumeClaimTemplates", []):
        claim.setdefault("metadata", {}).setdefault("labels", {})["trial"] = name
    kept.append(o)
print(json.dumps({"apiVersion": "v1", "kind": "List", "items": kept}))
'
}

# Every object of a name the trial uses that is there and is not the trial's.
foreign() {  # MANIFEST-JSON
    python3 -c 'import json,sys; [print(o["kind"].lower()+"/"+o["metadata"]["name"]) for o in json.load(sys.stdin)["items"]]' < "$1" |
        while read -r obj; do
            have="$("${kube[@]}" get "$obj" -o 'jsonpath={.metadata.labels.trial}' 2>/dev/null)" || continue
            [ "$have" = "$name" ] || echo "$obj"
        done
}

case "$action" in
up)
    mkdir -p "$work"
    started=$(date +%s)
    memory_image="$(build_image memory cmem "$src/deploy/Dockerfile" "${TRIAL_MEMORY_BASE:-alpine/git:v2.49.1}")"
    dispatcher_image="$(build_image dispatcher dispatcher "$src/deploy/dispatcher/Dockerfile" "${TRIAL_DISPATCHER_BASE:-postgres:17-alpine}")"
    echo "built $memory_image and $dispatcher_image in $(( $(date +%s) - started ))s"
    render > "$work/manifest.json"
    clash="$(foreign "$work/manifest.json")"
    for s in cmem-tokens dispatcher-tokens dispatcher-db; do
        have="$("${kube[@]}" get "secret/$s" -o 'jsonpath={.metadata.labels.trial}' 2>/dev/null)" || continue
        [ "$have" = "$name" ] || clash="$clash secret/$s"
    done
    [ -z "$(echo "$clash" | tr -d ' \n')" ] || die "these are in $namespace and are not this trial's, so it does not start: $(echo $clash)"
    # The secrets as a deploy makes them, by the same code; and the move of
    # an old-style one, which a deploy does before it applies.
    secret_was="$("${kube[@]}" get secret dispatcher-tokens -o 'jsonpath={.metadata.resourceVersion}' 2>/dev/null || true)"
    # From another checkout, what is there is left as it is: that checkout's
    # deploy would not move it (the way back of deploy/RECOVERY.md).
    if [ "$src" = . ] || ! "${kube[@]}" get secret dispatcher-tokens >/dev/null 2>&1; then
        deploy_sh secrets
    fi
    if [ "${3:-}" = --old-secret ]; then
        # In its place, one in the form a deploy made before ADR 0051: the
        # owner's token in the clear, and in the file, as the owner had it.
        owner="$(openssl rand -hex 24)" agent="$(openssl rand -hex 24)" backup="$(openssl rand -hex 24)" host="$(openssl rand -hex 24)"
        (umask 077 && printf '%s\n' "$owner" > "$work/dispatcher-owner.token")
        "${kube[@]}" delete secret dispatcher-tokens >/dev/null
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: dispatcher-tokens
  namespace: $namespace
  labels: {trial: "$name"}
stringData:
  DISPATCHER_TOKENS: "owner=$owner,agent=$agent,backup=$backup"
  owner: "$owner"
  agent: "$agent"
  backup: "$backup"
  host: "$host"
EOF
        echo "made the dispatcher's token secret in its form from before ADR 0051"
    fi
    for s in cmem-tokens dispatcher-tokens dispatcher-db; do "${kube[@]}" label --overwrite "secret/$s" "$label" >/dev/null; done
    "${kube[@]}" apply -f "$work/manifest.json" >/dev/null
    # As a deploy does: a dispatcher that was running reads a changed secret only when it starts again.
    if [ "$secret_was" != "$("${kube[@]}" get secret dispatcher-tokens -o 'jsonpath={.metadata.resourceVersion}')" ] && [ -n "$secret_was" ]; then
        "${kube[@]}" rollout restart deployment/dispatcher >/dev/null
    fi
    "${kube[@]}" rollout status statefulset/cmem --timeout=180s >/dev/null
    "${kube[@]}" rollout status statefulset/dispatcher-db --timeout=180s >/dev/null
    "${kube[@]}" rollout status deployment/dispatcher --timeout=180s >/dev/null ||
        { "${kube[@]}" logs deployment/dispatcher --tail=20 >&2 || true; die "the dispatcher did not start"; }
    echo "the trial is up in $namespace after $(( $(date +%s) - started ))s"
    ;;
check)
    # The memory service answers, through a port forwarded for the check.
    log="$(mktemp)"
    "${kube[@]}" port-forward service/cmem :8377 > "$log" 2>&1 &
    forward=$!
    for _ in $(seq 50); do grep -q '^Forwarding from 127' "$log" && break; sleep 0.2; done
    port="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$log" | head -1)"
    ok=0; curl -fsS -m 5 -o /dev/null "http://127.0.0.1:$port/healthz" && ok=1
    kill "$forward" 2>/dev/null; wait "$forward" 2>/dev/null || true; rm -f "$log"
    [ "$ok" = 1 ] || die "the memory service does not answer"
    echo "the memory service answers"
    deploy_sh dispatch status >/dev/null && echo "the dispatcher answers the owner's client"
    id="probe-$(date +%H%M%S)"
    if ! out="$(deploy_sh dispatch submit probe "$id" 2>&1)"; then
        echo "$out" >&2
        [ "$account" != default ] || die "the dispatcher cannot start a Job here: it runs as the namespace's default account, which may not, and cannot reach the cluster's API (agent.sandbox.trial, ADR 0054)"
        die "the dispatcher did not take the probe task"
    fi
    for _ in $(seq 60); do
        state="$(deploy_sh dispatch status "$id" 2>/dev/null | head -1 || true)"
        case "$state" in *": done"* | *": failed"*) break ;; esac
        sleep 2
    done
    echo "the probe task: ${state:-no answer}"
    ;;
deploy)
    deploy_sh "${@:3}"
    ;;
down)
    started=$(date +%s)
    "${kube[@]}" delete "$kinds" -l "$label" --wait=true --timeout=120s >/dev/null
    left="$("${kube[@]}" get "$kinds" -l "$label" -o name 2>/dev/null)"
    [ -z "$left" ] || die "still there: $(echo $left)"
    rm -rf "$work"
    echo "took the trial down in $(( $(date +%s) - started ))s, and removed $work; its images stay in $registry"
    ;;
*)
    die "say: up [--old-secret], check, deploy ..., or down"
    ;;
esac
