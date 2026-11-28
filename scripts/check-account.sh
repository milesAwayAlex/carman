#!/usr/bin/env bash
# The model's account at the proxy is changed from inside the cluster, and
# only when told (ADR 0078, the manual mode), end to end in a trial with
# its own proxy and meter in front of the stand-in, two made-up accounts
# there, and two standing agents, one of them named as allowed:
#
#   1. the meter holds both credentials and the account the secret named;
#      no rule runs;
#   2. an agent the installation does not name is refused;
#   3. with a session in the middle of a turn the agent that is named is
#      told which, and that every live conversation loses its prompt cache:
#      nothing changes without --now; with it the change is recorded with
#      who asked and why, the meter follows within a minute, and the next
#      call goes out on the other credential;
#   4. the meter is started again: it holds the account the log names;
#   5. a name the meter has no credential for is refused; the owner tells
#      it back.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-account.sh CONTEXT   stands the trial up, checks, leaves it up
#   scripts/trial.sh CONTEXT down                             afterwards
#
# TRIAL_AGENT_BASE is an image with the harness (a session has to be in a
# turn). No model is asked and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-account: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-account.sh CONTEXT"
[ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_AGENT_BASE, an image with the harness"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
step() { echo "ok: $*"; }
has() { grep -qF -- "$2" <<<"$1" || die "expected \"$2\" in:
$1"; }

values="$(mktemp)"
cat > "$values" <<'YAML'
agents:
  - name: named
    interactive: true
    model: claude-haiku-5-5
    storage: 1Gi
    resources:
      requests: {cpu: 100m, memory: 256Mi}
      limits: {memory: 640Mi}
  - name: other
    interactive: true
    model: claude-haiku-5-5
    storage: 1Gi
    resources:
      requests: {cpu: 100m, memory: 256Mi}
      limits: {memory: 640Mi}
egress:
  model:
    idle: 5s
    longest: 20s
    accounts:
      enabled: true
      mode: manual
      agents: [named]
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_ACCOUNTS="first second" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
"${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null
"${kube[@]}" rollout status "deployment/${pre}model" --timeout=180s >/dev/null
[ -n "$("${kube[@]}" get secret "${pre}model-credential" -o 'jsonpath={.data.account\.second}')" ] ||
    die "the trial's secret has no credential for a second account: it was made before this check; take the trial down and run again"
for a in named other; do
    for _ in $(seq 60); do "${kube[@]}" get pod "${pre}agent-$a-0" >/dev/null 2>&1 && break; sleep 2; done
    "${kube[@]}" wait --for=condition=ready "pod/${pre}agent-$a-0" --timeout=300s >/dev/null
done
in_pod() { "${kube[@]}" exec "${pre}agent-$1-0" -- sh -c "$2"; }
for _ in $(seq 90); do in_pod other 'test -e /tmp/agent-session-ready' 2>/dev/null && break; sleep 2; done
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
ask() { in_pod named 'dispatcher stand-in ask' 2>&1 || true; }
seen() { in_pod named "curl -s --noproxy '*' http://${pre}model.$namespace.svc:8080/stand-in/seen" 2>&1 || true; }
asked_of() { seen | python3 -c 'import json,sys; print(json.load(sys.stdin).get("by_account", {}).get(sys.argv[1], 0))' "$1"; }
meter_log() { "${kube[@]}" logs "deployment/${pre}egress" -c meter --tail=-1 2>&1; }
holds() {  # NAME [SECONDS]: wait until the meter says it holds it
    local out
    for _ in $(seq "${2:-30}"); do
        out="$(owner account 2>/dev/null || true)"
        grep -qF "the meter holds account $1 " <<<"$out" && { echo "$out"; return 0; }
        sleep 2
    done
    die "the meter did not come to hold account $1: $out
$(meter_log | tail -8)"
}

# 1. Both credentials, the first held, no rule.
out="$(holds first)"
has "$out" "has credentials for: first, second"
has "$(meter_log)" "changes account only when told"
out="$(ask)"; has "$out" "status 200"
[ "$(asked_of first)" -ge 1 ] && [ "$(asked_of second)" -eq 0 ] || die "calls did not go out on the first account only: $(seen)"
! grep -q "stand-in-first-\|stand-in-second-" <<<"$(meter_log)" || die "the meter's log holds a credential"
step "the meter holds both accounts' credentials and the first account; calls go out on it, nothing is read of the other, and its log names no credential"

# 2. An agent the installation does not name.
out="$(in_pod other "dispatcher account hold second 'because I say so'" 2>&1 || true)"
has "$out" "egress.model.accounts.agents: named"
has "$(in_pod other 'dispatcher account' 2>&1)" "the meter holds account first"
step "an agent the installation does not name is refused, and can still ask which account is held"

# 3. A session in the middle of a turn: told, and nothing without --now.
in_pod other 'agent-open work "$HOME/work"' >/dev/null
for _ in $(seq 45); do owner peers 2>/dev/null | grep -E '^other/work ' | grep -q "idle" && break; sleep 2; done
owner send other/work 'Say ok slowly.
stand-in: slow 45' >/dev/null
for _ in $(seq 20); do owner peers 2>/dev/null | grep -E '^other/work ' | grep -q "busy" && break; sleep 1; done
out="$(in_pod named "dispatcher account hold second 'its week ends first'" 2>&1 || true)"
has "$out" "loses the prompt cache of every live conversation"
has "$out" "other/work"
has "$out" "--now"
[ -z "$(owner log --type model.account --last 5 2>/dev/null | grep -F '"to":"second"' || true)" ] || die "the account was changed without --now"
out="$(in_pod named "dispatcher account hold second 'its week ends first' --now" 2>&1)"
has "$out" "the proxy is told to hold account second"
has "$out" "loses its prompt cache"
has "$out" "other/work"
ev="$(owner log --type model.account --last 5 | grep -F '"to":"second"')"
has "$ev" '"by":"agent/named"'
has "$ev" "its week ends first"
has "$ev" '"from":"first"'
holds second >/dev/null
before="$(asked_of second)"
out="$(ask)"; has "$out" "status 200"
[ "$(asked_of second)" -gt "$before" ] || die "after the change a call did not go out on the second account: $(seen)"
step "with a session in a turn the named agent was told which and that the prompt cache is lost, and nothing changed; with --now the change was recorded with who and why, the meter followed, and the next call went out on the second account"

# 4. The meter started again holds what the log names.
for _ in $(seq 45); do owner peers 2>/dev/null | grep -E '^other/work ' | grep -q "idle" && break; sleep 2; done
"${kube[@]}" delete pod -l "app=egress,trial=$name" --wait=false >/dev/null
sleep 3
"${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null
holds second 45 >/dev/null
before="$(asked_of second)" first_before="$(asked_of first)"
for _ in $(seq 10); do out="$(ask)"; grep -q "status 200" <<<"$out" && break; sleep 3; done
has "$out" "status 200"
[ "$(asked_of second)" -gt "$before" ] && [ "$(asked_of first)" -eq "$first_before" ] || die "after its restart the meter did not send calls out on the second account: $(seen)"
step "the meter, started again, holds the account the log names (not the one the secret named first)"

# 5. A name it has not; and the owner tells it back.
out="$(owner account hold third 'no such' 2>&1 || true)"
has "$out" "no credential for an account"
out="$(owner account hold first 'back to the first')"
has "$out" "the proxy is told to hold account first"
holds first >/dev/null
has "$(owner account)" "said by owner: back to the first"
step "a name the meter has no credential for is refused; the owner told it back to the first and the meter followed"
echo "check-account: all passed; scripts/trial.sh $context down takes the trial down"
