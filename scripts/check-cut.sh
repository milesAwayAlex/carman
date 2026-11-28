#!/usr/bin/env bash
# A turn cut by an error of the model's API is not left hanging (ADR
# 0080), end to end in a trial with
# a real harness and no model: a standing agent's pod with one session, in
# front of the stand-in, which drops the connection in the middle of its
# answers as a proxy that restarts does.
#
#   1. every answer of a turn is cut: the harness tries again for some
#      minutes, then ends the turn with an API error. No Stop hook runs;
#      its StopFailure hook does: the session is recorded as cut, shown as
#      idle, and listens again;
#   2. a minute later the dispatcher tells it to go on; the message wakes
#      it as any other, and the turn that follows ends well;
#   3. a message sent to it afterwards is delivered as before.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-cut.sh CONTEXT   stands the trial up, checks, leaves it up (about eight minutes)
#   scripts/trial.sh CONTEXT down                         afterwards
#
# TRIAL_AGENT_BASE is an image with the harness. Nothing is asked of a
# model and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-cut: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-cut.sh CONTEXT"
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
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
pod="${pre}agent-desk-0"
for _ in $(seq 60); do "${kube[@]}" get pod "$pod" >/dev/null 2>&1 && break; sleep 2; done
"${kube[@]}" wait --for=condition=ready "pod/$pod" --timeout=300s >/dev/null
in_pod() { "${kube[@]}" exec "$pod" -- sh -c "$1"; }
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
for _ in $(seq 90); do in_pod 'test -e /tmp/agent-session-ready' 2>/dev/null && break; sleep 2; done
in_pod 'test -e /tmp/agent-session-ready' || die "the pod's sessions were not ready; its log: $("${kube[@]}" logs "$pod" --tail=20 2>&1)"
in_pod 'grep -q StopFailure "$HOME/.claude/settings.json"' || die "the pod's settings have no StopFailure hook: its image is from before this check; take the trial down and run again"
in_pod 'agent-open cut "$HOME/work"' >/dev/null

peer() { owner peers 2>/dev/null | grep -E '^desk/cut ' || true; }
await_peer() {  # WORDS [NOT-WORDS] [SECONDS]
    local line
    for _ in $(seq "${3:-60}"); do
        line="$(peer)"
        if grep -qF -- "$1" <<<"$line" && { [ -z "${2:-}" ] || ! grep -qF -- "$2" <<<"$line"; }; then echo "$line"; return 0; fi
        sleep 2
    done
    die "peers did not come to say \"$1\"${2:+ without \"$2\"} of desk/cut; it says: $(peer)"
}
cuts() { owner log --type session.cut --last 20 2>/dev/null | grep -c '"cuts"' || true; }
screen() { in_pod 't() { "$(cat /tmp/agent-tmux)" -S "/tmp/tmux-$(id -u)/default" "$@"; }; t capture-pane -p -t main:=cut' 2>/dev/null | grep -v '^$' | tail -n 12; }
await_peer "idle" "not listening" 90 >/dev/null

# 1. Every answer of the turn is cut. Twenty: more than a turn asks for
# before it gives up (ten tries of a stream, then whole answers), fewer
# than two turns do, so the turn after it ends well.
sent="$(owner send desk/cut 'Say ok.
stand-in: cut 20')"
has "$sent" " delivered"
echo "waiting for the harness to give up on the turn: about three and a half minutes"
for _ in $(seq 150); do [ "$(cuts)" -ge 1 ] && break; sleep 2; done
[ "$(cuts)" -eq 1 ] || die "no session.cut came; the window shows:
$(screen)"
cut="$(owner log --type session.cut --last 5)"
has "$cut" '"cuts":1'
has "$cut" '"name":"cut"'
has "$(screen)" "API Error"
await_peer "idle" "not listening" 20 >/dev/null
step "with every answer cut the harness gave up after its tries and ended the turn on an API error; its StopFailure hook ran: the session is recorded as cut, idle, and listening"

# 2. Told to go on, and the turn that follows ends well.
told=""
for _ in $(seq 60); do
    told="$(owner log --type message.sent --last 20 2>/dev/null | grep -F '"from":"dispatcher"' | grep -F 'cut by an error' || true)"
    [ -n "$told" ] && break
    sleep 2
done
has "$told" "Go on with what you were doing"
id="$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$told" | tail -1)"
out=""
for _ in $(seq 30); do out="$(owner message "$id" 2>/dev/null || true)"; grep -qF "message.delivered" <<<"$out" && break; sleep 2; done
has "$out" "message.delivered"
echo "waiting for the turn that follows: the stand-in cuts a few more answers first"
for _ in $(seq 120); do grep -qF "● ok" <<<"$(screen)" && break; sleep 2; done
has "$(screen)" "● ok"
await_peer "idle" "not listening" 30 >/dev/null
[ "$(cuts)" -eq 1 ] || die "the turn after the message was cut too: $(owner log --type session.cut --last 5)"
step "a minute after the cut the dispatcher told the session to go on ($id); the message woke it and the turn that followed ended well"

# 3. As before.
sent="$(owner send desk/cut 'Say ok once more.')"
has "$sent" " delivered"
await_peer "idle" "not listening" 60 >/dev/null
[ "$(cuts)" -eq 1 ] || die "a later turn was recorded as cut"
step "a message sent afterwards woke it and was answered as before"
echo "check-cut: all passed; scripts/trial.sh $context down takes the trial down"
