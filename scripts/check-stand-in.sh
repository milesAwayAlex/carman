#!/usr/bin/env bash
# The stand-in for the model's API (dispatcher stand-in, ADR 0068) set
# beside the API itself: the harness is run once against each on one small
# prompt, and the answers' shapes compared: a stream's events, each one's
# fields in the order sent, the headers that carry the plan's standing.
# The stand-in is what a trial's proxy and meter are tried against; it is
# written from what the meter reads and falls behind as the API changes.
#
#   TRIAL_MODEL=http://egress.NAMESPACE.svc:3131 TRIAL_AGENT_BASE=IMAGE scripts/check-stand-in.sh CONTEXT
#   scripts/trial.sh CONTEXT down              afterwards
#
# TRIAL_MODEL is the installation's listener for trials (ADR 0065), and
# TRIAL_AGENT_BASE an image with the harness. The harness asks, because the
# API takes a plan's credential from it alone; its one run makes a request
# or two of the cheapest model, counted there as trial/NAME/compare. To run when the API or the
# harness has changed, and before trusting a check that passed on the
# stand-in alone.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-stand-in: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_MODEL=URL scripts/check-stand-in.sh CONTEXT"
api="${TRIAL_MODEL:-}"
[ -n "$api" ] && [ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_MODEL, the installation's listener for trials, and TRIAL_AGENT_BASE, an image with the harness"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")

# A trial with a proxy of its own has the stand-in; its meter is not asked
# here. One agent, so that the trial makes the image with the harness.
values="$(mktemp)"
printf 'agents:\n  - name: compare\n    takesMessages: true\n    tools: all\n    models: [claude-haiku-5-5]\n' > "$values"
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
"${kube[@]}" rollout status "deployment/${pre}model" --timeout=180s >/dev/null
image="$("${kube[@]}" get cronjob "${pre}agent-compare" -o 'jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].image}')"
# A pod the installation's meter knows as an agent of this trial, with the
# credential the trial made up for its stand-in.
"${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}compare
  labels: {trial: "$name", app: "${pre}compare", agent: "compare", reaches-the-model: "true"}
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: $image
      command: ["sleep", "600"]
      resources: {requests: {cpu: 50m, memory: 128Mi}, limits: {memory: 768Mi}}
      env:
        - name: STAND_IN_TOKEN
          valueFrom: {secretKeyRef: {name: ${pre}model-credential, key: token}}
YAML
"${kube[@]}" wait --for=condition=ready "pod/${pre}compare" --timeout=120s >/dev/null
status=0
"${kube[@]}" exec "${pre}compare" -- dispatcher ready -- dispatcher stand-in compare --api "$api" --stand-in "http://${pre}model:8080" || status=$?
"${kube[@]}" delete pod "${pre}compare" --wait=false >/dev/null
[ "$status" -ne 4 ] || die "the stand-in says something the API does not (above); change cmd/dispatcher/standin.go to the API's shape"
[ "$status" -eq 0 ] || die "the two could not be compared (above)"
echo "the stand-in's answers are the API's in shape, in the trial $name; lines marked behind are what the API has more. Take it down with: scripts/trial.sh $context down"
