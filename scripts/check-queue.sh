#!/usr/bin/env bash
# The owner's queue worked through by itself (ADR
# new-the-queue-runs-itself), end to end with a real harness and no model:
# a trial with a proxy of its own in front of the stand-in (ADR 0068).
# Three items are added, the second marked hard; the strongest model is
# refused before anything starts and comes back half way.
#
# Tried: the queue starts the first item by itself on the model that is
# not the strongest; the hard item is held, with the reason shown, and
# does not hold the third, which runs; each ends done with the first line
# of its run's answer; when the strongest model answers again the hard
# item runs, on it; and an item whose run fails is failed, said so, and
# not taken again.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-queue.sh CONTEXT    stands the trial up, checks, and leaves it up
#   scripts/trial.sh CONTEXT down                            afterwards
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-queue: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-queue.sh CONTEXT"
[ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_AGENT_BASE, an image with the harness"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
step() { echo "ok: $*"; }

values="$(mktemp)"
cat > "$values" <<'YAML'
agents:
  - name: worker
    takesMessages: true
    tools: all
    models: [claude-haiku-5-5, claude-opus-5-5]
    deadlineSeconds: 300
dispatcher:
  maxRunning: 3
  queue:
    enabled: true
    agent: worker
    running: 1
    model: claude-haiku-5-5
    strongest: claude-opus-5-5
egress:
  model:
    idle: 5s
    longest: 20s
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
"${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null
image="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
list() { owner priority list; }
# The stand-in's standing, and a pod that asks the model as an agent's does.
standing() { "${kube[@]}" exec "${pre}caller" -- wget -q -O - --header 'content-type: application/json' --post-data "$1" "http://${pre}model:8080/stand-in/standing" >/dev/null; }
"${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}caller
  labels: {trial: "$name", app: "${pre}caller", agent: "caller", app.kubernetes.io/component: agent}
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
"${kube[@]}" wait --for=condition=ready "pod/${pre}caller" --timeout=120s >/dev/null
await() {  # WORDS [SECONDS]: the list, once it has a line with the words
    local out
    for _ in $(seq "${2:-60}"); do
        out="$(list 2>/dev/null || true)"
        if grep -qE -- "$1" <<<"$out"; then return 0; fi
        sleep 3
    done
    die "the list never said \"$1\"; it says:
$(list)
and the log:
$(owner log --last 25 | cut -c1-260)"
}

# The strongest model is refused for the plan's limit: one call of it is
# refused, which the meter records as a reading of that model.
standing '{"status":"rejected","windows":{"7d":{"utilization":1,"reset_in":86400,"status":"rejected"}}}'
"${kube[@]}" exec "${pre}caller" -- dispatcher ready -- dispatcher stand-in ask --model claude-opus-5-5 >/dev/null 2>&1 || true
standing '{"status":"allowed","windows":{"7d":{"utilization":0.2,"reset_in":86400,"status":"allowed"}}}'
for _ in $(seq 30); do owner log --type usage.observed --last 20 | grep -q '"model":"claude-opus-5-5".*rejected' && break; sleep 2; done
# And the plan's shared windows are read again from an answer that was
# not refused: it is the one model's limit that is spent, not the week.
"${kube[@]}" exec "${pre}caller" -- dispatcher stand-in ask --model claude-haiku-5-5 >/dev/null
for _ in $(seq 30); do owner log --type usage.observed --last 20 | grep -q '"model":"claude-haiku-5-5"' && break; sleep 2; done

owner priority set work --share 1 --default >/dev/null
owner priority add work 'Say what one and one make.' >/dev/null
owner priority add work 'A hard one: say what two and two make.' --hard >/dev/null
owner priority add work 'Say what three and three make.
stand-in: use Bash {"command":"echo six"}' >/dev/null

await 'work-1 +done .*by the queue: ok'
await 'work-3 +done .*by the queue: ok'
out="$(list)"
grep -qE 'work-2 +waiting' <<<"$out" && grep -q 'held: claude-opus-5-5 is refused' <<<"$out" || die "the hard item is not held for its model, with the reason:
$out"
models="$(owner log --type queue.started --last 10 | sed -n 's/.*"item":"\([^"]*\)".*"model":"\([^"]*\)".*/\1=\2/p' | tr '\n' ' ')"
[ "$models" = "work-1=claude-haiku-5-5 work-3=claude-haiku-5-5 " ] || die "the queue started: $models"
step "the queue started the first and the third item by itself, one at a time, on the model that is not the strongest, and each is done with the first line of its answer"
step "the hard item waits and did not hold the third: $(grep -A1 -E 'work-2 +waiting' <<<"$out" | tail -1 | sed 's/^ *//' | cut -c1-150)"

# The strongest model answers again: a reading of it that is not a refusal.
"${kube[@]}" exec "${pre}caller" -- dispatcher stand-in ask --model claude-opus-5-5 >/dev/null
for _ in $(seq 40); do owner status | grep -q 'claude-opus-5-5' && ! list | grep -q 'held: claude-opus-5-5 is refused' && break; sleep 3; done
await 'work-2 +done .*by the queue: ok' 80
owner log --type queue.started --last 10 | grep -q '"item":"work-2".*"model":"claude-opus-5-5"' || die "the hard item did not run on the strongest model: $(owner log --type queue.started --last 3)"
used="$(owner log --type model.usage --last 60 | grep -c '"model":"claude-opus-5-5"' || true)"
step "the strongest model answered again and the hard item ran on it (the trial's meter has $used record(s) of that model)"

# An item whose run fails: failed, said so, and not taken again.
owner priority add work 'This one is cut short.
stand-in: use Bash {"command":"kill -9 $PPID"}' >/dev/null
await 'work-4 +failed'
sleep 25
[ "$(owner log --type queue.started --last 20 | grep -c '"item":"work-4"')" = 1 ] || die "a failed item was started again"
step "an item whose run was cut short is failed and was not taken again: $(list | grep -E 'work-4 +failed' | sed 's/^ *//' | cut -c1-140)"
list
echo "the queue runs itself in the trial $name. Take it down with: scripts/trial.sh $context down"
