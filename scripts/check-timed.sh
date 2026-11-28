#!/usr/bin/env bash
# A message at a time (ADR new-a-message-at-a-time), end to end in a trial
# with a real harness and no model: a standing agent's pod with one session,
# in front of the stand-in for the model's API (ADR 0068).
#
#   1. the session asks for a message to itself in a little while, and for
#      another, which it cancels; `dispatcher timed` lists them;
#   2. the trial's dispatcher is restarted before the time;
#   3. at the time the message is sent and wakes the idle session, as any
#      message does; the cancelled one is not sent.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-timed.sh CONTEXT   stands the trial up, checks, leaves it up
#   scripts/trial.sh CONTEXT down                           afterwards
#
# TRIAL_AGENT_BASE is an image with the harness, the installation's warm
# image for one. Nothing is asked of a model and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-timed: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-timed.sh CONTEXT"
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
  - name: desk
    interactive: true
    model: claude-haiku-5-5
    storage: 1Gi
    resources:
      requests: {cpu: 100m, memory: 256Mi}
      limits: {memory: 768Mi}
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="${TRIAL_VALUES:-$values}" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
pod="${pre}agent-desk-0"
for _ in $(seq 60); do "${kube[@]}" get pod "$pod" >/dev/null 2>&1 && break; sleep 2; done
"${kube[@]}" wait --for=condition=ready "pod/$pod" --timeout=300s >/dev/null

in_pod() { "${kube[@]}" exec "$pod" -- sh -c "$1"; }
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
for _ in $(seq 90); do in_pod 'test -e /tmp/agent-session-ready' 2>/dev/null && break; sleep 2; done
in_pod 'test -e /tmp/agent-session-ready' || die "the pod's sessions were not ready; its log: $("${kube[@]}" logs "$pod" --tail=20 2>&1)"
in_pod 'agent-open timer "$HOME/work"' >/dev/null

peer() { owner peers 2>/dev/null | grep -E '^desk/timer ' || true; }
await_peer() {  # WORDS [NOT-WORDS] [SECONDS]
    local line
    for _ in $(seq "${3:-60}"); do
        line="$(peer)"
        if grep -qF -- "$1" <<<"$line" && { [ -z "${2:-}" ] || ! grep -qF -- "$2" <<<"$line"; }; then echo "$line"; return 0; fi
        sleep 2
    done
    die "peers did not come to say \"$1\"${2:+ without \"$2\"} of desk/timer; it says: $(peer)"
}
await_peer "idle" "not listening" 90 >/dev/null
# The session's id, from the dispatcher; its commands, as it runs them.
sid="$(owner peers 2>/dev/null | grep -E '^desk/timer ' | grep -oE 'session/[0-9a-f-]+' | head -1 | cut -d/ -f2)"
[ -n "$sid" ] || sid="$(in_pod 'ls /tmp/agent-harness | grep -v -e "^attend" -e "^\." | head -1')"
[ -n "$sid" ] || die "no id for the session desk/timer: $(peer)"
as_session() { in_pod "DISPATCHER_SESSION=$sid dispatcher $1"; }

# 1. Asked for, listed, one cancelled.
asked="$(as_session "send --at 100s desk/timer 'It is the time you said. Say ok.'")"
has "$asked" " timed: to be sent to session/$sid at "
id="${asked%% *}"
other="$(as_session "send --at 100s desk/timer 'This one is cancelled.'")"
oid="${other%% *}"
list="$(as_session timed)"
has "$list" "$id"
has "$list" "$oid"
has "$(owner timed)" "$id"
has "$(as_session "timed cancel $oid")" "$oid cancelled"
list="$(as_session timed)"
has "$list" "$id"
! grep -qF "$oid" <<<"$list" || die "the cancelled message is still listed: $list"
has "$(owner message "$id")" "(timed)"
bad="$(as_session "send --at whenever desk/timer 'x'" 2>&1 || true)"
has "$bad" "not a time"
owner dump | grep -qF "message.timed/$id" || die "the dispatcher's dump, which the backup copies, does not hold what was asked"
step "the session asked for a message to itself at a time ($id) and cancelled another ($oid); timed lists what waits, and the dispatcher's dump holds it"

# 2. The trial's dispatcher is started again before the time.
"${kube[@]}" rollout restart "deployment/${pre}dispatcher" >/dev/null
"${kube[@]}" rollout status "deployment/${pre}dispatcher" --timeout=120s >/dev/null
for _ in $(seq 30); do owner timed >/dev/null 2>&1 && break; sleep 2; done
has "$(owner timed)" "$id"
step "the trial's dispatcher was restarted before the time: the message still waits"

# 3. At the time it is sent, and wakes the session.
out=""
for _ in $(seq 100); do
    out="$(owner message "$id" 2>/dev/null || true)"
    grep -qF "message.delivered" <<<"$out" && break
    sleep 2
done
has "$out" "message.timed.done"
has "$out" '"how":"sent"'
has "$out" "message.delivered"
has "$out" "Sent at the time its sender asked for"
grep -qE '"how":"(wake|prompt|stop)"' <<<"$out" || die "the message was not taken by the session's hooks: $out"
await_peer "idle" "not listening" >/dev/null
out="$(owner message "$oid")"
has "$out" "(cancelled)"
! grep -qF "message.sent" <<<"$out" || die "the cancelled message was sent: $out"
has "$(as_session timed)" "no message waits"
step "at its time the message was sent and the idle session took it; the cancelled one was not sent"
echo "check-timed: all passed; scripts/trial.sh $context down takes the trial down"
