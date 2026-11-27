#!/usr/bin/env bash
# A trial with an egress proxy and meter of its own (ADR
# 0068), end to end with no model: the
# trial's proxy and meter, built from this checkout, in front of a stand-in
# for the model's API. What is tried: a request passes with the meter's
# credential on it and is recorded; a pod that is no agent's is refused; the
# account's standing is read from the answers' headers, and a refusal for a
# limit is a reading (ADR 0064); an agent over its limit at the proxy is
# held (ADR 0064); and a changed credential is taken up with no restart
# (ADR 0055). Nothing of the installation's own proxy is touched.
#
#   scripts/check-trial-proxy.sh CONTEXT       stands the trial up, checks, and leaves it up
#   scripts/trial.sh CONTEXT down              afterwards
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-trial-proxy: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: scripts/check-trial-proxy.sh CONTEXT"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
step() { echo "ok: $*"; }
has() { grep -qF -- "$2" <<<"$1" || die "expected \"$2\" in:
$1"; }

# Tallies that close soon, so that a check need not wait two minutes for a record.
values="$(mktemp)"
printf 'egress:\n  model:\n    idle: 5s\n    longest: 20s\n' > "$values"
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
"${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null
"${kube[@]}" rollout status "deployment/${pre}model" --timeout=180s >/dev/null
image="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"
# A pod that leaves through the trial's proxy as an agent's pod does, and
# says it is an agent's (and, for one step, does not).
pod() {  # NAME LABELS
    "${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}$1
  labels: {trial: "$name", app: "${pre}$1"$2}
spec:
  containers:
    - name: main
      image: $image
      command: ["sleep", "3600"]
      resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}
      env:
        - {name: HTTPS_PROXY, value: "http://${pre}egress:3128"}
        - {name: EGRESS_CA, value: /etc/agent/egress/ca.crt}
        - {name: CLAUDE_CODE_OAUTH_TOKEN, value: held-by-the-trial-proxy}
      volumeMounts: [{name: ca, mountPath: /etc/agent/egress, readOnly: true}]
  volumes:
    - name: ca
      configMap: {name: ${pre}egress-ca}
YAML
}
pod caller ', agent: "caller"'
"${kube[@]}" wait --for=condition=ready "pod/${pre}caller" --timeout=120s >/dev/null
sleep 2   # the network knows a new pod a moment after it starts

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
events() { owner log --type "$1" --last 100; }
await() {  # TYPE WORDS [SECONDS]
    local out
    for _ in $(seq "${3:-60}"); do
        out="$(events "$1" 2>/dev/null || true)"
        if grep -qF -- "$2" <<<"$out"; then grep -F -- "$2" <<<"$out" | tail -1; return 0; fi
        sleep 2
    done
    die "no $1 with \"$2\" came; the log has:
$(owner log --last 30)"
}
ask() { "${kube[@]}" exec "${pre}$1" -- dispatcher stand-in ask "${@:2}" 2>&1 || true; }
standin() { "${kube[@]}" exec "${pre}caller" -- sh -c "wget -q -O - $* http://${pre}model:8080$path" 2>&1 || true; }
seen() { path=/stand-in/seen standin; }
standing() { path=/stand-in/standing standin "--header 'content-type: application/json' --post-data '$1'"; }

# Through the proxy, in the model's host's name, to the stand-in: the meter
# put its credential on the request (the stand-in takes no other).
out="$(ask caller)"; has "$out" "status 200"; has "$out" "text ok"
out="$(ask caller --stream)"; has "$out" "status 200"; has "$out" "text ok"
has "$(seen)" '"refused_for_the_credential":0'
has "$(seen)" '"requests":2'
step "an agent's pod asked the model's host through the trial's proxy, whole and streamed; the stand-in got the meter's credential, not the pod's placeholder"
"${kube[@]}" label pod "${pre}caller" agent- >/dev/null
sleep 8   # the meter reads the pod's labels again within a few seconds
out="$(ask caller)"; has "$out" "status 402"
has "$(seen)" '"requests":2'
"${kube[@]}" label pod "${pre}caller" agent=caller >/dev/null
sleep 8
step "the same pod, no longer saying it is an agent's, is refused by the trial's meter, and nothing reaches the stand-in"

# What the meter recorded, in the trial's own log: the pod's use, and the
# account's standing as the answers' headers gave it (ADR 0064).
usage="$(await model.usage '"agent":"caller"' 60)"
has "$usage" '"output_tokens":10'
has "$usage" '"requests":2'
reading="$(await usage.observed 'five_hour' 60)"
step "the trial's meter recorded the pod's use (2 requests, 10 tokens out) and read the account's standing from the answers"

# The plan's limit, as the API refuses for it: the caller is told, and the
# refusal is a reading.
standing '{"status":"rejected","windows":{"5h":{"utilization":1,"reset_in":1800,"status":"rejected"}}}'
out="$(ask caller)"; has "$out" "status 429"
await usage.observed 'rejected' 60 >/dev/null
standing '{"status":"allowed","windows":{"5h":{"utilization":0.1,"reset_in":1800,"status":"allowed"}}}'
step "a call refused for the plan's limit came back as 429, and is in the log as a reading"

# A limit of the agent's own at the proxy (ADR 0064), set by the owner with
# no deploy: over it, the meter holds the agent and the stand-in is not asked.
owner limit caller --day 1 >/dev/null
before="$(seen)"
held=""
for _ in $(seq 40); do
    out="$(ask caller)"
    if grep -q "status 4" <<<"$out" && ! grep -q "status 429" <<<"$out"; then held="$out"; break; fi
    sleep 3
done
[ -n "$held" ] || die "the agent was not held to its limit at the proxy; the last answer: $out"
owner limit caller off >/dev/null
step "over its limit at the proxy the agent was held: $(sed -n 's/^text //p' <<<"$held" | cut -c1-140)"
for _ in $(seq 40); do out="$(ask caller)"; grep -q "status 200" <<<"$out" && break; sleep 3; done
has "$out" "status 200"
step "with the limit lifted it passes again"

# The credential changes (ADR 0055): the secret is replaced; the meter and
# the stand-in each take the new one up when the cluster swaps the mounted
# file, with no restart, and requests pass again.
restarts="$("${kube[@]}" get pod -l "app=egress,trial=$name" -o 'jsonpath={.items[0].status.containerStatuses[*].restartCount}')"
"${kube[@]}" patch secret "${pre}model-credential" --type merge -p "{\"stringData\":{\"token\":\"stand-in-changed-$(date +%s)\"}}" >/dev/null
changed=""
for _ in $(seq 100); do
    if "${kube[@]}" logs -l "app=egress,trial=$name" -c meter --tail=200 2>/dev/null | grep -q "the model's credential changed"; then changed=1; break; fi
    sleep 3
done
[ -n "$changed" ] || die "the meter did not take the changed credential up in five minutes"
for _ in $(seq 60); do out="$(ask caller)"; grep -q "status 200" <<<"$out" && break; sleep 3; done
has "$out" "status 200"
[ "$("${kube[@]}" get pod -l "app=egress,trial=$name" -o 'jsonpath={.items[0].status.containerStatuses[*].restartCount}')" = "$restarts" ] || die "the proxy's pod restarted"
step "the credential was changed in the secret; the meter took it up with no restart, and requests pass with the new one"
echo "a trial's own proxy and meter work end to end in the trial $name; take it down with: scripts/trial.sh $context down"
