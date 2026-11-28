#!/usr/bin/env bash
# The proxy switches the model's account by itself (ADR
# 0078), end to end in a trial with
# its own proxy and meter in front of the stand-in for the model's API, and
# two made-up accounts there, each with a standing of its own:
#
#   1. the meter holds both credentials, begins on the one the secret
#      named, and reads the one it does not hold;
#   2. the other account's week is made to end sooner: the meter goes to
#      it, the change is in the trial's log with its readings, the owner
#      is told in one line, and the next call goes out on its credential;
#   3. that account's five-hour window is filled: the meter goes back to
#      the first until the reset, not before the least time between
#      switches;
#   4. the window is emptied, as at its reset: the meter returns;
#   5. one model's own weekly limit is spent on the account held: that
#      model's requests go out on the other account, the others stay,
#      and the change is recorded and told once;
#   6. readings say which account they are of, and `dispatch plan` shows
#      both accounts and what the proxy did and why.
#
#   scripts/check-switch.sh CONTEXT       stands the trial up, checks, and leaves it up
#   scripts/trial.sh CONTEXT down         afterwards
#
# No model is asked and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-switch: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: scripts/check-switch.sh CONTEXT"
name="$(scripts/trial.sh "$context" name)"
pre="$name-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
step() { echo "ok: $*"; }
has() { grep -qF -- "$2" <<<"$1" || die "expected \"$2\" in:
$1"; }

# Short times, so that the check need not wait half an hour for a switch
# or an hour for a reading.
values="$(mktemp)"
cat > "$values" <<'YAML'
egress:
  model:
    idle: 5s
    longest: 20s
    accounts:
      enabled: true
      readModel: claude-haiku-5-5
      hold: 20s
      readEvery: 15s
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_ACCOUNTS="first second" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
"${kube[@]}" rollout status "deployment/${pre}egress" --timeout=180s >/dev/null
"${kube[@]}" rollout status "deployment/${pre}model" --timeout=180s >/dev/null
[ -n "$("${kube[@]}" get secret "${pre}model-credential" -o 'jsonpath={.data.account\.second}')" ] ||
    die "the trial's secret has no credential for a second account: it was made before this check; take the trial down and run again"
image="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"
"${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}caller
  labels: {trial: "$name", app: "${pre}caller", agent: "caller"}
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
sleep 2   # the network knows a new pod a moment after it starts

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
events() { owner log --type "$1" --last 100; }
await() {  # TYPE WORDS [SECONDS]
    local out
    for _ in $(seq "${3:-45}"); do
        out="$(events "$1" 2>/dev/null || true)"
        if grep -qF -- "$2" <<<"$out"; then grep -F -- "$2" <<<"$out" | tail -1; return 0; fi
        sleep 2
    done
    die "no $1 with \"$2\" came; the log has:
$(owner log --last 30)
the meter's log:
$("${kube[@]}" logs "deployment/${pre}egress" -c meter --tail=20 2>&1)"
}
ask() { "${kube[@]}" exec "${pre}caller" -- dispatcher stand-in ask "$@" 2>&1 || true; }
standin() { "${kube[@]}" exec "${pre}caller" -- sh -c "wget -q -O - $* http://${pre}model:8080$path" 2>&1 || true; }
seen() { path=/stand-in/seen standin; }
standing() { path=/stand-in/standing standin "--header 'content-type: application/json' --post-data '$1'"; }
asked_of() { seen | python3 -c 'import json,sys; print(json.load(sys.stdin).get("by_account", {}).get(sys.argv[1], 0))' "$1"; }
meter_log() { "${kube[@]}" logs "deployment/${pre}egress" -c meter --tail=-1 2>&1; }

# 1. Both credentials, the first to begin with, and the other read.
out="$(ask)"; has "$out" "status 200"
has "$(seen)" '"refused_for_the_credential":0'
log="$(meter_log)"
has "$log" "the meter holds several accounts for the model"
has "$log" "accounts=first,second"
! grep -q "stand-in-first-\|stand-in-second-" <<<"$log" || die "the meter's log holds a credential"
await usage.observed '"account":"second"' >/dev/null
await usage.observed '"account":"first"' >/dev/null
[ "$(asked_of second)" -ge 1 ] || die "the account not held was not read: $(seen)"
step "the meter holds both accounts' credentials, sent an agent's call out on the first, and read the standing of the one it does not hold; its log names no credential"

# 2. The second's week ends sooner and has room: the meter goes to it.
standing '{"account":"second","windows":{"7d":{"utilization":0.4,"reset_in":86400}}}'
sw="$(await model.account '"to":"second"')"
has "$sw" '"rule":"week"'
has "$sw" '"by":"proxy"'
has "$sw" '"from":"first"'
has "$sw" '"standings"'
before="$(asked_of second)"
out="$(ask)"; has "$out" "status 200"
[ "$(asked_of second)" -gt "$before" ] || die "after the switch a call did not go out on the second account: $(seen)"
inbox=""
for _ in $(seq 20); do inbox="$(owner inbox 2>/dev/null || true)"; grep -qF "now holds the model's account second" <<<"$inbox" && break; sleep 2; done
has "$inbox" "The proxy now holds the model's account second (it held first): the week of account second ends soonest"
step "with the second account's week ending sooner the meter went to it by itself: recorded with its readings, the owner told in one line, and the next call went out on the second's credential"

# 3. Its five-hour window fills: to the first, until the reset.
standing '{"account":"second","windows":{"5h":{"utilization":0.9,"reset_in":3000}}}'
out="$(ask)"; has "$out" "status 200"
sw="$(await model.account '"rule":"five-hour"')"
has "$sw" '"to":"first"'
has "$sw" "the five-hour window of account second is at 90%"
before="$(asked_of first)"
out="$(ask)"; has "$out" "status 200"
[ "$(asked_of first)" -gt "$before" ] || die "after the five-hour switch a call did not go out on the first account: $(seen)"
step "with the second account's five-hour window at 90% the meter went to the first, and calls go out on it"

# 4. The window empties, as at its reset: back to the second.
standing '{"account":"second","windows":{"5h":{"utilization":0.05,"reset_in":18000}}}'
n="$(events model.account | grep -c '"to":"second"' || true)"
for _ in $(seq 45); do
    [ "$(events model.account | grep -c '"to":"second"' || true)" -gt "$n" ] && break
    sleep 2
done
[ "$(events model.account | grep -c '"to":"second"' || true)" -gt "$n" ] || die "the meter did not return to the second account: $(events model.account)
$(meter_log | tail -10)"
step "with the window empty again the meter returned to the second account"

# No two switches closer than the least time between them (20s here).
events model.account | python3 -c '
import json, sys, datetime
ts = []
for line in sys.stdin:
    i = line.find("{")
    if i < 0: continue
    try: d = json.loads(line[i:])
    except ValueError: continue
    if d.get("by") == "proxy":
        t = d["at"]; t = t[:19] + "+00:00"
        ts.append(datetime.datetime.fromisoformat(t).timestamp())
ts.sort()
gaps = [b - a for a, b in zip(ts, ts[1:])]
assert len(ts) >= 3, ts
assert all(g >= 19 for g in gaps), gaps
print("gaps between switches, seconds:", [int(g) for g in gaps])' || die "two switches were closer than the least time between them"
step "no two switches were closer than the least time between them"

# 5. One model's own limit is spent on the account held: that model's
# requests go out on the other account, the rest stay.
held="second" other="first"
standing '{"account":"second","windows":{"7d_opus":{"utilization":0.98,"reset_in":86400}}}'
standing '{"account":"first","windows":{"7d_opus":{"utilization":0.2,"reset_in":400000}}}'
by() { seen | python3 -c 'import json,sys; print(json.load(sys.stdin).get("by_account_and_model", {}).get(sys.argv[1], 0))' "$1"; }
out="$(ask --model claude-opus-5-5)"; has "$out" "status 200"   # this answer says the limit is at 98%
o_other="$(by "$other/claude-opus-5-5")" h_held="$(by "$held/claude-haiku-5-5")" h_other="$(by "$other/claude-haiku-5-5")"
for _ in 1 2 3; do out="$(ask --model claude-opus-5-5)"; has "$out" "status 200"; done
out="$(ask --model claude-haiku-5-5)"; has "$out" "status 200"
[ "$(by "$other/claude-opus-5-5")" -eq $((o_other + 3)) ] || die "the model whose limit is spent on $held did not go out on $other: $(seen)"
[ "$(by "$held/claude-haiku-5-5")" -gt "$h_held" ] || die "another model did not stay on $held: $(seen)"
: "$h_other"
aside="$(await model.account.for '"to":"first"')"
has "$aside" '"model":"claude-opus-5-5"'
has "$aside" '"rule":"model"'
has "$aside" "the limit of claude-opus-5-5 on account second is at 98%"
[ "$(events model.account.for | grep -c '"to":"first"')" -eq 1 ] || die "the change was recorded more than once: $(events model.account.for)"
for _ in $(seq 20); do inbox="$(owner inbox 2>/dev/null || true)"; grep -qF "requests of claude-opus-5-5 on the model's account first" <<<"$inbox" && break; sleep 2; done
has "$inbox" "The proxy now sends requests of claude-opus-5-5 on the model's account first"
[ "$(grep -c "requests of claude-opus-5-5 on the model's account first" <<<"$inbox")" -eq 1 ] || die "the owner was told more than once: $inbox"
step "with one model's own limit at 98% on the held account its requests went out on the other account and another model's stayed; recorded once, the owner told once"

# 6. What the owner sees.
plan="$(owner plan)"
has "$plan" "account first"
has "$plan" "account second (held by the proxy"
has "$plan" "by the proxy (rule: five-hour): the five-hour window of account second is at 90%"
has "$plan" "by the proxy (rule: week)"
has "$plan" "requests of claude-opus-5-5 to account first, by the proxy (rule: model)"
step "dispatch plan shows both accounts' standing, which one is held, and each switch with its rule and reason"
echo "check-switch: all passed; scripts/trial.sh $context down takes the trial down"
