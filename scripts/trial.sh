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
#   scripts/trial.sh CONTEXT down [NAME] delete everything the trial made, and only that;
#                                        another's only when NAME is said
#   scripts/trial.sh CONTEXT list        which trials are up in the namespace, whose, since
#                                        when, and what of its quota each holds
#   scripts/trial.sh CONTEXT name        this session's trial name
#
# TRIAL_DEFINITION=DIR, a git repository standing for an installation's
# definition: `up` puts its bundle in the trial as a deploy does, and runs a
# pod `definition-reader` that has it as an agent given it does (ADR 0056);
# `deploy definition apply ID` applies a proposal to DIR.
#
# TRIAL_TICK=1: a class tick that does nothing (true, in the dispatcher's
# image), its CronJob a suspended template on a schedule of every minute,
# which the dispatcher starts every minute as it starts a nightly job (ADR 0059).
# TRIAL_VALUES=FILE: values on top of the trial's own (deploy/values/trial.yaml),
# for a trial of something they leave off.
#
# The namespace is the context's own unless TRIAL_NAMESPACE names one.
# Several trials stand side by side there. A trial's name is TRIAL_NAME, or
# made from the agent and session it is run from (AGENT_NAME and the
# dispatcher's session name), so that it is the same for the whole session.
# Every object's name is the chart's with the prefix NAME- (NAME-dispatcher,
# NAME-cmem-tokens), the deploy script is told the prefix
# (CARMAN_OBJECT_PREFIX), and the dispatcher lists and watches only what
# carries the label trial=NAME (DISPATCHER_SELECTOR). Everything made carries
# that label, and `down` deletes only what carries it. `up` refuses where an
# object of one of its names is there and is not the trial's, and where the
# namespace's quota has no room for it.
#
# Images are built with BuildKit (buildctl, BUILDKIT_HOST) and pushed to
# TRIAL_REGISTRY (localhost:5050), as TRIAL_NAME-memory and
# TRIAL_NAME-dispatcher; the registry keeps them after `down`.
# The trial's owner token is in .local/trial/NAME/dispatcher-owner.token,
# with what the builds made; `down` removes that directory.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "trial: $*" >&2; exit 1; }

context="${1:-}" action="${2:-}"
[ -n "$context" ] && [ -n "$action" ] || die "say: scripts/trial.sh CONTEXT up|check|deploy|down|list|name"
# A name of the session's own: its agent's and session's, else the user's
# and the checkout's; short, so the chart's names still fit with it.
default_name() {
    local who what
    if [ -n "${AGENT_NAME:-}" ]; then
        who="$AGENT_NAME" what="${DISPATCHER_SESSION_NAME:-${DISPATCHER_SESSION:-${CLAUDE_CODE_SESSION_ID:-}}}"
        what="${what:0:8}"
    else
        who="$(id -un)" what="$(pwd -P | shasum -a 256 | cut -c1-6)"
    fi
    printf '%s-%s' "$who" "$what"
}
name="$(printf '%s' "${TRIAL_NAME:-$(default_name)}" | tr 'A-Z_./' 'a-z---' | tr -cd 'a-z0-9-' | sed 's/^-*//; s/-*$//')"
[ "${#name}" -le 24 ] || name="${name:0:17}-$(printf '%s' "$name" | shasum -a 256 | cut -c1-6)"
[ -n "$name" ] || die "no name for the trial; set TRIAL_NAME"
case "$name" in carman*) die "TRIAL_NAME may not start with the project's name" ;; esac
pre="$name-"
# Who has the trial, for `list`.
holder="${AGENT_NAME:+$AGENT_NAME/}${DISPATCHER_SESSION_NAME:-${DISPATCHER_SESSION:-$(id -un)}}"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
[ -n "$namespace" ] || die "the context $context names no namespace; set TRIAL_NAMESPACE"
kube=(kubectl --context "$context" --namespace "$namespace")
registry="${TRIAL_REGISTRY:-localhost:5050}"
label="trial=$name"
work=".local/trial/$name"
# What the chart makes that a sandbox lets nobody but the installation make.
refused='ServiceAccount Role RoleBinding NetworkPolicy'
# The kinds a trial leaves, for `down`.
kinds=deployments,statefulsets,services,configmaps,secrets,cronjobs,jobs,pods,persistentvolumeclaims
# The secrets the deploy script makes, and the trial labels after.
made_secrets="${pre}cmem-tokens ${pre}dispatcher-tokens ${pre}dispatcher-db"
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
    CARMAN_NAMESPACE="$namespace" CARMAN_OBJECT_PREFIX="$pre" CARMAN_OWNER_TOKEN_FILE="$work/dispatcher-owner.token" CARMAN_INSTALLATION="${TRIAL_DEFINITION:-$work/no-definition}" scripts/deploy.sh "$context" "$@"
}

# Builds an image of a binary of this checkout with its own Dockerfile, and
# prints its name: a trial runs the images an installation would.
build_image() {  # NAME CMD DOCKERFILE
    local dir="$work/build/$1" arch tag ref log
    rm -rf "$dir" && mkdir -p "$dir"
    arch="$(kubectl --context "$context" get --raw /version 2>/dev/null | sed -n 's/.*"platform": *"linux\/\([a-z0-9]*\)".*/\1/p')"
    [ -n "$arch" ] || arch="$(uname -m | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
    (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -o "$OLDPWD/$dir/$2-$arch" "./cmd/$2")
    cp "$3" "$dir/Dockerfile"
    tag="$(cat "$dir"/* | shasum -a 256 | cut -c1-12)"
    ref="$registry/$name-$1:$tag"
    log="$dir/build.log"
    buildctl build --progress plain --frontend dockerfile.v0 --local context="$dir" --local dockerfile="$dir" \
        --opt platform="linux/$arch" --output "type=image,name=$ref,push=true" > "$log" 2>&1 ||
        { tail -20 "$log" >&2; die "$1's image did not build"; }
    echo "$ref"
}

# The chart, rendered for the trial and made fit for the namespace: without
# what only the installation may make, every pod as the trial's account,
# every name and reference to one prefixed, every object and pod labelled,
# and each selector narrowed to the label.
render() {
    local tick=()
    [ -z "${TRIAL_TICK:-}" ] ||
        tick=(--set-json 'dispatcher.schedules=[{"name":"tick","class":"tick","cron":"* * * * *","startWithin":"5m","timeout":"5m","priority":100}]')
    helm template "$name" "$src/charts/carman" --namespace "$namespace" -f deploy/values/trial.yaml ${TRIAL_VALUES:+-f "$TRIAL_VALUES"} \
        --set "memory.image=$memory_image" --set "dispatcher.image=$dispatcher_image" --set "agent.image=$dispatcher_image" "${tick[@]}" |
        kubectl --context "$context" create --dry-run=client -o json -f - |
        TRIAL_REFUSED="$refused" TRIAL_LABEL="$name" TRIAL_PREFIX="$pre" TRIAL_OWNER="$holder" TRIAL_ACCOUNT="$account" TRIAL_IMAGE="$dispatcher_image" python3 -c '
import json, os, re, sys
refused = os.environ["TRIAL_REFUSED"].split()
name, account, pre = os.environ["TRIAL_LABEL"], os.environ["TRIAL_ACCOUNT"], os.environ["TRIAL_PREFIX"]
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
# job says what it was handed (its TASK_ variables) and ends well, in the
# dispatcher image.
out.append({"apiVersion": "batch/v1", "kind": "CronJob", "metadata": {"name": "probe"},
    "spec": {"schedule": "0 0 1 1 *", "suspend": True, "jobTemplate": {"spec": {"backoffLimit": 0, "template": {"spec": {
        "restartPolicy": "Never", "containers": [{"name": "probe", "image": os.environ["TRIAL_IMAGE"], "command": ["/bin/sh", "-c", "env | grep ^TASK_ | sort; true"]}]}}}}}})
if os.environ.get("TRIAL_TICK"):
    out.append({"apiVersion": "batch/v1", "kind": "CronJob", "metadata": {"name": "tick"},
        "spec": {"schedule": "* * * * *", "suspend": True, "jobTemplate": {"spec": {"backoffLimit": 0, "ttlSecondsAfterFinished": 600,
            "template": {"spec": {"restartPolicy": "Never", "containers": [{"name": "tick", "image": os.environ["TRIAL_IMAGE"], "command": ["/bin/true"]}]}}}}}})
out = [o for o in out if o["kind"] not in refused]
services = {o["metadata"]["name"] for o in out if o["kind"] == "Service"}
classes = {o["metadata"]["name"] for o in out if o["kind"] == "CronJob"}
host = re.compile(r"(^|//)(" + "|".join(map(re.escape, sorted(services, key=len, reverse=True))) + r")(?=$|[.:/])")
def refs(v):
    # Every secret, config map and claim a pod names, and every service it
    # reaches by its name, gets the prefix.
    if isinstance(v, list):
        for x in v:
            refs(x)
    if not isinstance(v, dict):
        return
    for k in ("secretKeyRef", "configMapKeyRef", "secretRef", "configMapRef", "configMap", "secret"):
        ref = v.get(k)
        if isinstance(ref, dict):
            for f in ("name", "secretName"):
                if isinstance(ref.get(f), str):
                    ref[f] = pre + ref[f]
    if isinstance(v.get("persistentVolumeClaim"), dict):
        v["persistentVolumeClaim"]["claimName"] = pre + v["persistentVolumeClaim"]["claimName"]
    if isinstance(v.get("value"), str) and "name" in v:
        n, val = v["name"], v["value"]
        if n == "DISPATCHER_SCHEDULES":
            s = json.loads(val)
            for e in s:
                if e.get("class") in classes:
                    e["class"] = pre + e["class"]
            v["value"] = json.dumps(s)
        elif n == "DISPATCHER_RULES":
            # A rule starts a class by its name here, and tells a template by it.
            s = json.loads(val)
            for e in s:
                if e.get("start") in classes:
                    e["start"] = pre + e["start"]
                where = e.get("when", {}).get("where") or {}
                if where.get("template") in classes:
                    where["template"] = pre + where["template"]
            v["value"] = json.dumps(s)
        elif n == "DISPATCHER_NOTICE_CLASS" and val in classes:
            v["value"] = pre + val
        elif services and ("//" in val or n.endswith("HOST")):
            v["value"] = host.sub(lambda m: m.group(1) + pre + m.group(2), val)
    for k, x in v.items():
        if k not in ("metadata", "selector", "matchLabels"):
            refs(x)
kept = []
for o in out:
    o["metadata"]["name"] = pre + o["metadata"]["name"]
    o["metadata"].setdefault("labels", {})["trial"] = name
    o["metadata"].setdefault("annotations", {})["trial/owner"] = os.environ["TRIAL_OWNER"]
    spec = o.get("spec", {})
    pod = spec.get("template") or spec.get("jobTemplate", {}).get("spec", {}).get("template")
    if "jobTemplate" in spec:
        # The dispatcher gives a Job it starts the labels of this template.
        spec["jobTemplate"].setdefault("metadata", {}).setdefault("labels", {})["trial"] = name
    if pod is not None:
        pod.setdefault("metadata", {}).setdefault("labels", {})["trial"] = name
        pod["spec"]["serviceAccountName"] = "default"
        if o["kind"] == "Deployment" and o["metadata"]["name"] == pre + "dispatcher" and account != "default":
            pod["spec"]["serviceAccountName"] = account
            pod["metadata"]["labels"]["reaches-the-api"] = "true"
    if pod is not None:
        refs(pod["spec"])
        if o["kind"] == "Deployment" and o["metadata"]["name"] == pre + "dispatcher":
            for c in pod["spec"]["containers"]:
                c.setdefault("env", []).append({"name": "DISPATCHER_SELECTOR", "value": "trial=" + name})
    if o["kind"] == "StatefulSet":
        spec["serviceName"] = pre + spec["serviceName"]
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

# Quantities and the use of a namespace's quota, for `up` and `list`.
quota_py='
import json, re, sys
def qty(v):
    m = re.fullmatch(r"([0-9.]+)([a-zA-Z]*)", str(v))
    units = {"": 1, "m": 1e-3, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "Ti": 2**40}
    return float(m.group(1)) * units[m.group(2)]
def show(res, v):
    if res.endswith("cpu"):
        return "%dm" % round(v * 1000)
    if "memory" in res or "storage" in res:
        return "%dMi" % round(v / 2**20)
    return "%d" % v
def pod_use(spec, defaults):
    # What a pod counts against the quota, with the defaults of the namespace
    # where a container says nothing.
    use = {"pods": 1}
    for c in spec.get("containers", []):
        res = c.get("resources", {})
        for kind in ("requests", "limits"):
            given = dict(defaults.get(kind, {}))
            given.update(res.get(kind, {}))
            if kind == "requests":
                for k, v in res.get("limits", {}).items():
                    given.setdefault(k, v)
            for k, v in given.items():
                key = kind + "." + k
                use[key] = use.get(key, 0) + qty(v)
    return use
def add(a, b, n=1):
    for k, v in b.items():
        a[k] = a.get(k, 0) + v * n
def quotas(text):
    out = {}
    for q in json.loads(text)["items"]:
        for k, v in q.get("status", {}).get("hard", {}).items():
            key = {"cpu": "requests.cpu", "memory": "requests.memory"}.get(k, k)
            out[key] = (qty(v), qty(q["status"].get("used", {}).get(k, 0)))
    return out
def defaults(text):
    d = {}
    for lr in json.loads(text)["items"]:
        for l in lr["spec"].get("limits", []):
            if l.get("type") == "Container":
                d.setdefault("requests", {}).update(l.get("defaultRequest", {}))
                d.setdefault("limits", {}).update(l.get("default", {}))
    return d
'

case "$action" in
name)
    echo "$name"
    ;;
list)
    TRIAL_QUOTA="$("${kube[@]}" get resourcequota -o json)" python3 -c "$quota_py"'
import os
objs = json.load(sys.stdin)["items"]
trials = {}
for o in objs:
    m = o["metadata"]
    t = trials.setdefault(m["labels"]["trial"], {"since": m["creationTimestamp"], "owner": "", "use": {}})
    t["since"] = min(t["since"], m["creationTimestamp"])
    t["owner"] = t["owner"] or m.get("annotations", {}).get("trial/owner", "")
    if o["kind"] == "Pod" and o.get("status", {}).get("phase") in ("Pending", "Running"):
        add(t["use"], pod_use(o["spec"], {}))
q = quotas(os.environ["TRIAL_QUOTA"])
cols = [k for k in ("pods", "requests.cpu", "requests.memory", "limits.memory") if k in q]
print("%-26s %-28s %-21s %s" % ("TRIAL", "WHOSE", "SINCE", "  ".join(cols)))
for n, t in sorted(trials.items(), key=lambda x: x[1]["since"]):
    print("%-26s %-28s %-21s %s" % (n, t["owner"] or "(not said)", t["since"], "  ".join(show(k, t["use"].get(k, 0)).rjust(len(k)) for k in cols)))
if q:
    print("the quota, used of all:  " + ", ".join("%s %s of %s" % (k, show(k, q[k][1]), show(k, q[k][0])) for k in sorted(q)))
if not trials:
    print("no trial is up in this namespace")
' < <("${kube[@]}" get "$kinds" -l trial -o json)
    ;;
up)
    mkdir -p "$work"
    started=$(date +%s)
    memory_image="$(build_image memory cmem "$src/deploy/Dockerfile")"
    dispatcher_image="$(build_image dispatcher dispatcher "$src/deploy/dispatcher/Dockerfile")"
    echo "built $memory_image and $dispatcher_image in $(( $(date +%s) - started ))s"
    render > "$work/manifest.json"
    clash="$(foreign "$work/manifest.json")"
    for s in $made_secrets; do
        have="$("${kube[@]}" get "secret/$s" -o 'jsonpath={.metadata.labels.trial}' 2>/dev/null)" || continue
        [ "$have" = "$name" ] || clash="$clash secret/$s"
    done
    [ -z "$(echo "$clash" | tr -d ' \n')" ] || die "these are in $namespace and are not this trial's, so it does not start: $(echo $clash)"
    # Room in the quota for the trial's pods and one task's, unless it is up already.
    if ! "${kube[@]}" get "deployment/${pre}dispatcher" >/dev/null 2>&1; then
        short="$(TRIAL_QUOTA="$("${kube[@]}" get resourcequota -o json)" TRIAL_LIMITS="$("${kube[@]}" get limitrange -o json)" python3 -c "$quota_py"'
import os
d = defaults(os.environ["TRIAL_LIMITS"])
need = {}
for o in json.load(sys.stdin)["items"]:
    spec = o.get("spec", {})
    if o["kind"] in ("Deployment", "StatefulSet"):
        add(need, pod_use(spec["template"]["spec"], d), spec.get("replicas", 1))
    elif o["kind"] == "CronJob" and o["metadata"]["name"].endswith("-probe"):
        add(need, pod_use(spec["jobTemplate"]["spec"]["template"]["spec"], d))
for k, (hard, used) in sorted(quotas(os.environ["TRIAL_QUOTA"]).items()):
    if need.get(k, 0) > hard - used:
        print("%s: it needs %s, and %s of %s is free" % (k, show(k, need[k]), show(k, max(hard - used, 0)), show(k, hard)))
' < "$work/manifest.json")"
        [ -z "$short" ] || { "$0" "$context" list >&2 || true; die "the namespace's quota has no room for this trial, so it does not start: $(echo $short). Take down a trial of your own (down), or wait for another to go"; }
    fi
    # The secrets as a deploy makes them, by the same code; and the move of
    # an old-style one, which a deploy does before it applies.
    secret_was="$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.metadata.resourceVersion}' 2>/dev/null || true)"
    # From another checkout, what is there is left as it is: that checkout's
    # deploy would not move it (the way back of deploy/RECOVERY.md).
    if [ "$src" = . ] || ! "${kube[@]}" get secret ${pre}dispatcher-tokens >/dev/null 2>&1; then
        deploy_sh secrets
    fi
    if [ "${3:-}" = --old-secret ]; then
        # In its place, one in the form a deploy made before ADR 0051: the
        # owner's token in the clear, and in the file, as the owner had it.
        owner="$(openssl rand -hex 24)" agent="$(openssl rand -hex 24)" backup="$(openssl rand -hex 24)" host="$(openssl rand -hex 24)"
        (umask 077 && printf '%s\n' "$owner" > "$work/dispatcher-owner.token")
        "${kube[@]}" delete secret ${pre}dispatcher-tokens >/dev/null
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${pre}dispatcher-tokens
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
    for s in $made_secrets; do
        "${kube[@]}" label --overwrite "secret/$s" "$label" >/dev/null
        "${kube[@]}" annotate --overwrite "secret/$s" "trial/owner=$holder" >/dev/null
    done
    "${kube[@]}" apply -f "$work/manifest.json" >/dev/null
    # As a deploy does: a dispatcher that was running reads a changed secret only when it starts again.
    if [ "$secret_was" != "$("${kube[@]}" get secret ${pre}dispatcher-tokens -o 'jsonpath={.metadata.resourceVersion}')" ] && [ -n "$secret_was" ]; then
        "${kube[@]}" rollout restart deployment/${pre}dispatcher >/dev/null
    fi
    "${kube[@]}" rollout status statefulset/${pre}cmem --timeout=180s >/dev/null
    "${kube[@]}" rollout status statefulset/${pre}dispatcher-db --timeout=180s >/dev/null
    "${kube[@]}" rollout status deployment/${pre}dispatcher --timeout=180s >/dev/null ||
        { "${kube[@]}" logs deployment/${pre}dispatcher --tail=20 >&2 || true; die "the dispatcher did not start"; }
    if [ -n "${TRIAL_DEFINITION:-}" ]; then
        # The definition's bundle, as a deploy puts it, and a pod that has
        # it as an agent given it does (definition: true, ADR 0056): the
        # memory service's image, which has git and a shell.
        have="$("${kube[@]}" get configmap ${pre}installation -o 'jsonpath={.metadata.labels.trial}' 2>/dev/null)" && [ "$have" != "$name" ] &&
            die "a config map ${pre}installation is in $namespace and is not this trial's"
        deploy_sh definition put
        "${kube[@]}" label --overwrite configmap/${pre}installation "$label" >/dev/null
        "${kube[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}definition-reader
  namespace: $namespace
  labels: {trial: "$name"}
spec:
  restartPolicy: Never
  containers:
    - name: reader
      image: $memory_image
      command: ["sleep", "86400"]
      env: [{name: HOME, value: /tmp}, {name: AGENT_NAME, value: trial}]
      volumeMounts: [{name: definition, mountPath: /etc/installation, readOnly: true}]
  volumes:
    - name: definition
      configMap: {name: ${pre}installation, optional: true}
EOF
        "${kube[@]}" wait --for=condition=ready pod/${pre}definition-reader --timeout=120s >/dev/null
        echo "the trial has the definition at $TRIAL_DEFINITION, and the pod ${pre}definition-reader has it at /etc/installation"
    fi
    echo "the trial is up in $namespace after $(( $(date +%s) - started ))s"
    ;;
check)
    # The memory service answers, through a port forwarded for the check.
    log="$(mktemp)"
    "${kube[@]}" port-forward service/${pre}cmem :8377 > "$log" 2>&1 &
    forward=$!
    for _ in $(seq 50); do grep -q '^Forwarding from 127' "$log" && break; sleep 0.2; done
    port="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$log" | head -1)"
    ok=0; curl -fsS -m 5 -o /dev/null "http://127.0.0.1:$port/healthz" && ok=1
    kill "$forward" 2>/dev/null; wait "$forward" 2>/dev/null || true; rm -f "$log"
    [ "$ok" = 1 ] || die "the memory service does not answer"
    echo "the memory service answers"
    deploy_sh dispatch status >/dev/null && echo "the dispatcher answers the owner's client"
    id="${pre}probe-$(date +%H%M%S)"
    if ! out="$(deploy_sh dispatch submit "${pre}probe" "$id" 2>&1)"; then
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
    status=0; deploy_sh "${@:3}" || status=$?
    # What the deploy script made with the prefix (a secret, the definition's
    # config map) becomes the trial's, for `down`.
    for obj in $("${kube[@]}" get secrets,configmaps -l '!trial' -o name 2>/dev/null | grep "/$pre" || true); do
        "${kube[@]}" label "$obj" "$label" >/dev/null && "${kube[@]}" annotate --overwrite "$obj" "trial/owner=$holder" >/dev/null
    done
    exit "$status"
    ;;
down)
    # Another's trial only when its name is said.
    if [ -n "${3:-}" ] && [ "$3" != "$name" ]; then
        name="$3" label="trial=$3" work=".local/trial/$3"
        "${kube[@]}" get "$kinds" -l "$label" -o name 2>/dev/null | grep -q . || die "no trial $name is up in $namespace"
        echo "taking down the trial $name, which is not this session's"
    fi
    started=$(date +%s)
    "${kube[@]}" delete "$kinds" -l "$label" --wait=true --timeout=120s >/dev/null
    left="$("${kube[@]}" get "$kinds" -l "$label" -o name 2>/dev/null)"
    [ -z "$left" ] || die "still there: $(echo $left)"
    rm -rf "$work"
    echo "took the trial down in $(( $(date +%s) - started ))s, and removed $work; its images stay in $registry"
    ;;
*)
    die "say: up [--old-secret], check, deploy ..., down [NAME], list or name"
    ;;
esac
