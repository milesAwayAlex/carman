#!/usr/bin/env bash
# A trial whose agents reach the model (ADR new-a-trial-agent-reaches-the-model),
# end to end: an agent held to one command runs a routine on the cheapest
# model, started by a rule when a page this script changes is seen to have
# changed; two agents of the trial exchange a message; and the installation's
# meter refuses a pod that is no trial's agent, and a model trials may not use.
# It spends a little of the account: some tens of requests to the cheapest model.
#
#   TRIAL_MODEL=http://egress.NAMESPACE.svc:3131 TRIAL_AGENT_BASE=IMAGE scripts/check-trial-agents.sh CONTEXT
#   scripts/trial.sh CONTEXT down              afterwards
#
# TRIAL_MODEL is the installation's listener for trials (egress.model.trials)
# and TRIAL_AGENT_BASE an image with the harness, the installation's warm
# image. What the meter recorded is in the installation's own log, which its
# owner reads: scripts/deploy.sh CONTEXT dispatch log --type model.usage.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-trial-agents: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: scripts/check-trial-agents.sh CONTEXT"
[ -n "${TRIAL_MODEL:-}" ] && [ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_MODEL, the installation's listener for trials, and TRIAL_AGENT_BASE, an image with the harness"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
step() { echo "ok: $*"; }
has() { grep -qF -- "$2" <<<"$1" || die "expected \"$2\" in:
$1"; }

values="$(mktemp)"
sed "s|http://source:|http://${pre}source:|" deploy/values/trial-agents.yaml > "$values"
TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
image="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"
# A page to watch, and two pods to call the meter with: one that says it is
# an agent of this trial, one that says nothing of itself. Both may reach
# the listener; only the first is anybody to it.
pod() {  # NAME LABELS COMMAND
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
      command: ["sh", "-c", "$3"]
YAML
}
pod source "" "mkdir -p /tmp/w && echo 'price 100' > /tmp/w/index.html && while true; do { printf 'HTTP/1.0 200 OK\\\\r\\\\n\\\\r\\\\n'; cat /tmp/w/index.html; } | nc -l -p 8080 >/dev/null; done"
pod caller ', agent: "caller", reaches-the-model: "true"' "sleep 3600"
pod stray ', reaches-the-model: "true"' "sleep 3600"
"${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${pre}source
  labels: {trial: "$name"}
spec:
  selector: {app: "${pre}source"}
  ports: [{port: 8080}]
YAML
"${kube[@]}" wait --for=condition=ready "pod/${pre}source" "pod/${pre}caller" "pod/${pre}stray" --timeout=120s >/dev/null
sleep 8   # a pod just made is refused its first connections for a moment

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
events() { owner log --type "$1" --last 100; }
await() {  # TYPE WORDS [SECONDS]
    local out
    for _ in $(seq "${3:-90}"); do
        out="$(events "$1" 2>/dev/null || true)"
        if grep -qF -- "$2" <<<"$out"; then grep -F -- "$2" <<<"$out" | tail -1; return 0; fi
        sleep 2
    done
    die "no $1 with \"$2\" came; the log has:
$(owner log --last 40)"
}
# What the meter answers a pod's request for a model: the status and the body.
ask() {  # POD MODEL
    "${kube[@]}" exec "${pre}$1" -- sh -c "wget -q -S -O - --header 'content-type: application/json' --header 'anthropic-version: 2023-06-01' --post-data '{\"model\":\"$2\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"Reply ok\"}]}' $TRIAL_MODEL/v1/messages 2>&1 | grep -E 'HTTP/|\"type\"' | head -3" || true
}

# The meter's own limits, before anything is spent.
out="$(ask stray claude-haiku-5-5)"; has "$out" " 403"
step "a pod of the sandbox that does not say it is a trial's agent is refused by the meter"
out="$(ask caller claude-opus-5-5)"; has "$out" " 403"
step "a trial's agent that asks for a model trials may not use is refused"

# A restricted agent, started by a rule from a watch, runs a routine on the model.
await watch.first '"watch":"page"' 120 >/dev/null
"${kube[@]}" exec "${pre}source" -- sh -c "echo 'price 101' > /tmp/w/index.html"
fired="$(await rule.fired '"rule":"page-changed"' 120)"
task="$(sed -n 's/.*"task":"\([^"]*\)".*/\1/p' <<<"$fired")"
for _ in $(seq 300); do
    state="$(owner status "$task" 2>/dev/null | head -1 || true)"
    case "$state" in *": done"* | *": failed"*) break ;; esac
    sleep 3
done
has "$state" ": done"
usage="$(await run.usage '"agent":"surveyor"' 30)"
has "$usage" "claude-haiku-5-5"
survey="$(events improvement.surveyed | tail -1)"
[ -n "$survey" ] || die "the surveyor's run ended and recorded no survey; its log: $("${kube[@]}" logs -l "app=agent-surveyor,trial=$name" --tail=30 2>&1)"
step "the page changed, the rule started the surveyor, held to one command, and its run on the cheapest model recorded a survey: $(sed -n 's/.*"report":"\([^"]\{0,160\}\).*/\1/p' <<<"$survey")"
step "its run's own count: $(sed -n 's/.*\("cost_usd":[0-9.]*\).*/\1/p' <<<"$usage")"

# Two agents exchange a message: the owner asks one to ask the other.
owner send asker "Ask the agent answerer what 17 plus 25 is: run exactly this once: dispatcher send answerer 'What is 17 plus 25? Answer with the number only.' Then say only that you asked. Do nothing else." >/dev/null
asked="$(await message.sent '"to":"agent/answerer"' 240)"
has "$asked" '"agent":"asker"'
for _ in $(seq 120); do
    answer="$(events message.sent | grep '"agent":"answerer"' | tail -1 || true)"
    [ -n "$answer" ] && break
    sleep 3
done
[ -n "${answer:-}" ] || die "the answerer did not answer; the log has: $(events message.sent)"
has "$answer" "42"
step "the agent asker wrote to the agent answerer, whose run answered: $(sed -n 's/.*"body":"\([^"]\{0,80\}\).*/\1/p' <<<"$answer")"
echo "a trial's agents reach the model in the trial $name. The meter's record is in the installation's own log, under trial/$name/surveyor, /asker and /answerer:"
echo "  scripts/deploy.sh CONTEXT dispatch log --type model.usage --last 30"
echo "take the trial down with: scripts/trial.sh $context down"
