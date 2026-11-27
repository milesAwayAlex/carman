#!/usr/bin/env bash
# The improvement loop (ADR 0058) end to end in a trial installation, with
# no model: evidence in, the list brought up to date, a brief for an item
# the owner agreed held by each rule in turn and then passed, and nothing
# sent to anyone. A pod stands in for the surveyor and one for an agent that
# reports; what the surveyor would decide, this script decides.
#
#   TRIAL_VALUES=deploy/values/trial-improve.yaml scripts/trial.sh CONTEXT up
#   scripts/check-improve.sh CONTEXT
#
# TRIAL_NAME and TRIAL_NAMESPACE as scripts/trial.sh takes them.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-improve: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: scripts/check-improve.sh CONTEXT"
name="${TRIAL_NAME:-trial}"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
image="$("${kube[@]}" get deployment dispatcher -o 'jsonpath={.spec.template.spec.containers[0].image}')" || die "no trial dispatcher in $namespace"
[ "$("${kube[@]}" get deployment dispatcher -o 'jsonpath={.metadata.labels.trial}')" = "$name" ] || die "the dispatcher in $namespace is not the trial $name"

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
# A pod the cluster vouches for as an agent: its label, and a token of its
# own for the dispatcher.
for agent in watch platform other; do
    "${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: stand-in-$agent
  labels: {trial: "$name", agent: "$agent"}
spec:
  restartPolicy: Never
  containers:
    - name: agent
      image: $image
      command: ["sleep", "3600"]
      env:
        - {name: DISPATCHER_URL, value: "http://dispatcher:8379"}
        - {name: AGENT_NAME, value: "$agent"}
      volumeMounts: [{name: identity, mountPath: /var/run/secrets/dispatcher, readOnly: true}]
  volumes:
    - name: identity
      projected:
        sources: [{serviceAccountToken: {audience: dispatcher, path: token, expirationSeconds: 3600}}]
YAML
done
"${kube[@]}" wait --for=condition=ready pod/stand-in-watch pod/stand-in-platform pod/stand-in-other --timeout=120s >/dev/null
as() { local agent="$1"; shift; "${kube[@]}" exec -i "stand-in-$agent" -- dispatcher "$@"; }
step() { echo "ok: $*"; }
has() { grep -qF -- "$2" <<<"$1" || die "expected \"$2\" in:
$1"; }
lacks() { ! grep -qF -- "$2" <<<"$1" || die "did not expect \"$2\" in:
$1"; }

# The list starts with what is known: the seed, written down by the owner.
out="$(owner improve import docs/improvements/seed.json)"; has "$out" "wrote down"
again="$(owner improve import docs/improvements/seed.json)"; has "$again" "wrote down 0 item(s)"
step "the seed is on the list, once: $out"

# Evidence: a run that failed, one that succeeded, a report from an agent
# whose words are evidence, and a message from one whose words are not.
stamp="$(date +%H%M%S)"
owner emit job.ended --id "check-$stamp-bad" --subject agent-example --data '{"job":"agent-example-1","template":"agent-example","started_by":"cronjob","outcome":"failed","reason":"DeadlineExceeded","ended":"2026-01-01T00:00:00Z"}' >/dev/null
owner emit job.ended --id "check-$stamp-good" --subject agent-example --data '{"job":"agent-example-2","template":"agent-example","started_by":"cronjob","outcome":"succeeded","ended":"2026-01-01T00:00:00Z"}' >/dev/null
as platform send owner "The slice is pushed. What was awkward: the check $stamp had to be run twice because the first port-forward died." >/dev/null
as other send owner "words of $stamp that are nobody's evidence" >/dev/null
# And what a deploy says it deployed, which only the owner's side may say.
printf '%s\n' "abc1234 The client retries its port-forward once" "def5678 An earlier change" | owner deployed "1111111111111111111111111111111111111111" --previous "2222222222222222222222222222222222222222" >/dev/null
! as platform emit installation.deployed --data '{"commit":"forged"}' >/dev/null 2>&1 || die "an agent recorded a deploy"
ev="$(as watch improve evidence)"
has "$ev" "the platform deployed is now 111111111111"
has "$ev" "abc1234 The client retries its port-forward once"
lacks "$ev" "forged"
has "$ev" "agent-example-1 (agent-example) failed: DeadlineExceeded"
lacks "$ev" "agent-example-2"
has "$ev" "the check $stamp had to be run twice"
lacks "$ev" "nobody's evidence"
has "$ev" "this sender's words are not evidence here"
through="$(sed -n 's/.* read through \([0-9]*\);.*/\1/p' <<<"$ev" | tail -1)"
[ -n "$through" ] || die "the evidence does not say where it read to: $ev"
step "the surveyor reads the evidence through $through: a failure, what was deployed, a listed sender's words, and of another sender only that it wrote"

# Who may: an agent the installation does not name reads nothing; an agent
# of the list reads the list and not the evidence.
! as other improve list >/dev/null 2>&1 || die "an agent the installation does not name reads the list"
as platform improve list >/dev/null || die "an agent of the list cannot read it"
! as platform improve evidence >/dev/null 2>&1 || die "an agent that is not the surveyor reads the evidence"
step "only the surveyor reads the evidence, and only named agents the list"

# The list updated: one new item, and the same id again refused.
item="port-forward-dies-in-checks-$stamp"
as watch improve note "$item" --title "A check survives its port-forward dying" --kind friction --size small \
    --why "A check that fails for the forward and passes on a second run teaches people to run things twice." \
    --takes "Retry the forward inside the deploy script's client, once." \
    --evidence "message from platform, $stamp: \"the check had to be run twice because the first port-forward died\"" >/dev/null
dup="$(as watch improve note "$item" --title x --why y --kind gap 2>&1 || true)"; has "$dup" "already"
as watch improve add "$item" --evidence "run agent-example-1 failed: DeadlineExceeded" >/dev/null
shown="$(owner improve show "$item")"; has "$shown" "noticed"; has "$shown" "DeadlineExceeded"
step "the list has the new item, once, with both pieces of evidence"

# Doing, within limits. Not agreed: held. Only the owner agrees.
brief="Make the deploy script's client retry its port-forward once.
Done when scripts/check-improve.sh passes with the first forward killed.
Work on a slice branch; do not merge or deploy; report what was built and what was awkward."
held() {  # WANT, then the brief's arguments
    local want="$1" out status=0; shift
    out="$(as watch improve brief "$@" 2>&1 <<<"$brief")" || status=$?
    [ "$status" = 5 ] || die "a brief that should be held ended $status: $out"
    has "$out" "$want"
}
held "waits for the owner's decision" "$item" --to agent/builder -
! as watch improve agree "$item" >/dev/null 2>&1 || die "the surveyor agreed to its own item"
owner improve agree "$item" >/dev/null
held "only to the addressees" "$item" --to agent/desk -
held "no usage reading" "$item" --to agent/builder -
now="$(date +%s)"
reading() { owner emit usage.observed --data "{\"model\":\"m\",\"rate_limit_info\":{\"status\":\"allowed\",\"unifiedWindows\":{\"five_hour\":{\"utilization\":$1,\"resetsAt\":$((now + 3600))},\"seven_day\":{\"utilization\":0.2,\"resetsAt\":$((now + 86400))}}}}" >/dev/null; }
reading 0.8
held "the five-hour window is at 80%" "$item" --to agent/builder -
reading 0.1
passed="$(as watch improve brief "$item" --to agent/builder - <<<"$brief")"
has "$passed" "recorded, not sent"
held "written already" "$item" --to agent/builder -
step "a brief is held until the owner agrees, the addressee is named and usage has room; then it passes and is recorded, not sent"
other_item="$(owner improve list --state noticed | sed -n 's/^  \([a-z0-9-]*\) .*/\1/p' | head -1)"
owner improve agree "$other_item" >/dev/null
held "budget" "$other_item" --to agent/builder -
owner improve reopen "$other_item" "the check's own" >/dev/null
step "a second brief in the same day is held by the budget"

# Telling the owner little: the survey records the one line; nothing goes.
said="$(as watch improve surveyed --through "$through" "Read to $through. Wrote down $item. One brief passed, not sent.")"
has "$said" "the owner was not told (notices from the improvement loop are off)"
has "$said" "wait for your decision"
[ "$(as watch improve evidence | sed -n 's/.* line(s) of evidence after \([0-9]*\),.*/\1/p')" = "$through" ] || die "the next survey does not start where this one read to"
step "the survey is recorded, and the next starts at $through"

# Nothing was sent to a person or an agent: no message to the builder, and
# the only messages to the owner are the two this check sent itself.
log="$(owner log --type message.sent --last 200)"
lacks "$log" '"to":"agent/builder"'
lacks "$log" "wait for your decision"
shown="$(owner improve show "$item")"; has "$shown" "agreed"; has "$shown" "not sent"
step "nothing was sent: the item is still agreed, its brief is on the list for the owner to read"
"${kube[@]}" delete pod stand-in-watch stand-in-platform stand-in-other --wait=false >/dev/null
echo "the improvement loop works end to end in the trial $name"
