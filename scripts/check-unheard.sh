#!/usr/bin/env bash
# A session that does not hear is noticed (ADR
# new-a-session-that-does-not-hear-is-noticed), end to end in a trial with a
# real harness and no model: a standing agent's pod with one session, in
# front of the stand-in for the model's API (ADR 0068).
#
#   1. the session's listener is ended by its process id: the session is
#      shown as idle, not listening; a message to it says so, the pod's
#      watcher types a line into its window, the turn's hook takes the
#      message, and the session listens again;
#   2. the same with a client looking at the window: nothing is typed, the
#      message says why, its sender is told after the time set
#      (dispatcher.messages.waitNotice, a minute here), and once the client
#      has gone the line is typed;
#   3. the same with a line left half typed in the window: nothing is typed
#      until the line is cleared;
#   4. with CHECK_EXPIRY=1: a client of the pod goes on showing a token
#      after it has expired; the dispatcher records it once, `dispatch
#      status` and `peers` show it, a message to the session says it, and
#      when the client stops that is recorded. A pod's token lasts ten
#      minutes at the least, so this part waits about twelve.
#
#   TRIAL_AGENT_BASE=IMAGE [CHECK_EXPIRY=1] scripts/check-unheard.sh CONTEXT   stands the trial up, checks, leaves it up
#   scripts/trial.sh CONTEXT down                                              afterwards
#
# TRIAL_AGENT_BASE is an image with the harness, the installation's warm
# image for one. Nothing is asked of a model and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-unheard: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-unheard.sh CONTEXT"
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
      limits: {memory: 1Gi}
dispatcher:
  messages:
    waitNotice: 1m
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"
pod="${pre}agent-desk-0"
if [ -n "${CHECK_EXPIRY:-}" ]; then
    # The pod's token for the dispatcher at its shortest life, ten minutes.
    "${kube[@]}" get statefulset "${pre}agent-desk" -o json | python3 -c '
import json, sys
o = json.load(sys.stdin)
changed = False
for v in o["spec"]["template"]["spec"]["volumes"]:
    for s in v.get("projected", {}).get("sources", []):
        t = s.get("serviceAccountToken")
        if t and t.get("audience") == "dispatcher" and t.get("expirationSeconds") != 600:
            t["expirationSeconds"] = 600
            changed = True
for k in ("status",):
    o.pop(k, None)
for k in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
    o["metadata"].pop(k, None)
print(json.dumps(o) if changed else "")' > "$values.sts"
    if [ -s "$values.sts" ]; then
        "${kube[@]}" apply -f "$values.sts" >/dev/null
        "${kube[@]}" delete pod "$pod" --wait=true >/dev/null 2>&1 || true
    fi
    rm -f "$values.sts"
fi
for _ in $(seq 60); do "${kube[@]}" get pod "$pod" >/dev/null 2>&1 && break; sleep 2; done
"${kube[@]}" wait --for=condition=ready "pod/$pod" --timeout=300s >/dev/null
started="$(date +%s)"

in_pod() { "${kube[@]}" exec "$pod" -- sh -c "$1"; }
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
for _ in $(seq 90); do in_pod 'test -e /tmp/agent-session-ready' 2>/dev/null && break; sleep 2; done
in_pod 'test -e /tmp/agent-session-ready' || die "the pod's sessions were not ready; its log: $("${kube[@]}" logs "$pod" --tail=20 2>&1)"
in_pod 'agent-open notes "$HOME/work"' >/dev/null

# What peers says of the session.
peer() { owner peers 2>/dev/null | grep -E '^desk/notes ' || true; }
await_peer() {  # WORDS [NOT-WORDS] [SECONDS]
    local line
    for _ in $(seq "${3:-60}"); do
        line="$(peer)"
        if grep -qF -- "$1" <<<"$line" && { [ -z "${2:-}" ] || ! grep -qF -- "$2" <<<"$line"; }; then echo "$line"; return 0; fi
        sleep 2
    done
    die "peers did not come to say \"$1\"${2:+ without \"$2\"} of desk/notes; it says: $(peer)
the watcher's log: $(in_pod 'tail -15 /tmp/agent-attend.log' 2>&1)"
}
# The session's id, and the ids of its listeners: the processes of the idle
# waiter that have the session's harness above them.
session() { in_pod 'ls /tmp/agent-harness | grep -v -e "^attend" -e "^\." | head -1'; }
listeners() {
    in_pod 'h="$(cut -d" " -f1 "/tmp/agent-harness/'"$1"'")"
for p in $(pgrep -f "dispatcher hook claude-code --wait"); do
    q="$p"
    while [ "${q:-0}" -gt 1 ]; do
        [ "$q" = "$h" ] && { echo "$p"; break; }
        q="$(awk "/^PPid:/ {print \$2}" "/proc/$q/status" 2>/dev/null)"
    done
done' | tr '\n' ' '
}
deafen() {  # ends the session's listeners by their ids, and waits until it shows
    local ids
    ids="$(listeners "$sid")"
    [ -n "${ids// /}" ] || die "the session has no listener to end; peers says: $(peer)"
    in_pod "kill $ids"
    await_peer "idle, not listening" >/dev/null
}
status_of() { owner message "$1" 2>/dev/null || true; }
await_message() {  # ID WORDS [SECONDS]
    local out
    for _ in $(seq "${3:-60}"); do
        out="$(status_of "$1")"
        grep -qF -- "$2" <<<"$out" && { echo "$out"; return 0; }
        sleep 2
    done
    die "message $1 did not come to say \"$2\":
$(status_of "$1")
the watcher's log: $(in_pod 'tail -15 /tmp/agent-attend.log' 2>&1)"
}
send() { owner send desk/notes "$1"; }   # prints: ID STATE: DETAIL

await_peer "idle" "not listening" 90 >/dev/null
sid="$(session)"
[ -n "$sid" ] || die "the pod has no record of the session's harness (/tmp/agent-harness)"
first="$(send 'Say ok.')"
has "$first" " delivered"
has "$first" "(wake)"
await_peer "idle" "not listening" >/dev/null
step "a session with its listener is woken by a message: ${first%% *}"

# 1. Its listener ended by its id: shown, said, put right.
deafen
step "its listener, ended by its process id, is not put back by the harness: peers says idle, not listening"
sent="$(send 'Say ok again.')"
id="${sent%% *}"
if ! grep -qF " delivered" <<<"$sent"; then
    has "$sent" "not listening"
    has "$sent" "watcher"
fi
out="$(await_message "$id" '"how":"prompt"' 60)"
has "$out" "not listening"
has "$(in_pod 'cat /tmp/agent-attend.log')" "a line was typed into its window"
await_peer "idle" "not listening" >/dev/null
[ -n "$(listeners "$sid" | tr -d ' ')" ] || die "the turn the line started left the session with no listener"
step "a message for it says it does not listen; the pod's watcher typed a line into its window, the turn's hook took the message ($id), and the session listens again"

# 2. A client looks at the window: nothing is typed, the sender is told.
tmux_pod='t() { "$(cat /tmp/agent-tmux)" -S "/tmp/tmux-$(id -u)/default" "$@"; }'
in_pod "$tmux_pod"'; t select-window -t main:=notes; "$(cat /tmp/agent-tmux)" -L person -f /dev/null new-session -d -x 120 -y 40 "$(cat /tmp/agent-tmux) -S /tmp/tmux-$(id -u)/default attach -t main"'
for _ in $(seq 30); do [ -n "$(in_pod "$tmux_pod"'; t list-clients' 2>/dev/null)" ] && break; sleep 1; done
deafen
sent="$(send 'Say ok a third time.')"
id="${sent%% *}"
out="$(await_message "$id" "a person's terminal is at its window" 60)"
! grep -qF "message.delivered" <<<"$out" || die "a line was typed into a window a client looks at: $out"
step "with a client at its window nothing is typed, and the message says why"
for _ in $(seq 75); do
    inbox="$(owner inbox 2>/dev/null || true)"
    grep -qF "Your message $id" <<<"$inbox" && break
    sleep 2
done
has "$inbox" "Your message $id"
has "$inbox" "has not been delivered"
has "$inbox" "a person's terminal is at its window"
has "$(status_of "$id")" "message.waited"
step "after the time set its sender is told once that it waits, and why"
in_pod '"$(cat /tmp/agent-tmux)" -L person kill-server'
await_message "$id" '"how":"prompt"' 60 >/dev/null
await_peer "idle" "not listening" >/dev/null
step "once the client has gone, the line is typed and the message taken ($id)"

# 3. A line left half typed: not sent off with the watcher's.
deafen
in_pod "$tmux_pod"'; t send-keys -t main:=notes -l "half a line a person left"'
sent="$(send 'Say ok a fourth time.')"
id="${sent%% *}"
out="$(await_message "$id" "not at an empty prompt" 60)"
! grep -qF "message.delivered" <<<"$out" || die "a line was typed over one left half typed: $out"
in_pod "$tmux_pod"'; t send-keys -t main:=notes C-u'
await_message "$id" '"how":"prompt"' 60 >/dev/null
step "a line left half typed in its window is not sent off with the watcher's; cleared, the line is typed ($id)"

if [ -z "${CHECK_EXPIRY:-}" ]; then
    echo "a session that does not hear is noticed and put right, in the trial $name (the part that lets a token expire was not asked for: CHECK_EXPIRY=1). Take it down with: scripts/trial.sh $context down"
    exit 0
fi

# 4. A client that goes on showing a token after it has expired.
in_pod 'cat "${DISPATCHER_IDENTITY:-/var/run/secrets/dispatcher/token}" > /tmp/old-token
(while true; do DISPATCHER_IDENTITY=/tmp/old-token dispatcher whoami >/dev/null 2>&1; sleep 1; done) >/dev/null 2>&1 &
echo $! > /tmp/old-client.pid'
left=$(( started + 600 + 30 + 75 - $(date +%s) ))
echo "waiting for the pod's token to expire and its refusals to go on: about $(( left > 0 ? left / 60 + 1 : 1 )) minute(s)"
for _ in $(seq 480); do
    refused="$(owner log --type client.refused --last 20 2>/dev/null | grep -F "$pod" || true)"
    [ -n "$refused" ] && break
    sleep 2
done
[ -n "$refused" ] || die "no client.refused for $pod came; the dispatcher's log: $("${kube[@]}" logs "deployment/${pre}dispatcher" --tail=10 2>&1)"
[ "$(wc -l <<<"$refused")" -eq 1 ] || die "the pod's refusals are recorded more than once: $refused"
has "$refused" '"agent":"desk"'
has "$refused" "expired"
has "$(owner status)" "refused again and again"
has "$(owner inbox)" "is refusing a client of pod $pod"
step "a client of the pod that shows an expired token again and again is recorded once, the owner is told, and dispatch status shows it"
deafen
has "$(await_peer "been refused by the dispatcher")" "idle, not listening"
sent="$(send 'Say ok a fifth time.')"
id="${sent%% *}"
grep -qF " delivered" <<<"$sent" || has "$sent" "been refused by the dispatcher"
await_message "$id" '"how":"prompt"' 60 >/dev/null
step "peers says of its session that does not listen that a client of its pod is refused, and so does a message to it; the pod's watcher, whose token is of now, still wakes it"
in_pod 'kill "$(cat /tmp/old-client.pid)"'
for _ in $(seq 120); do
    ended="$(owner log --type client.refused.ended --last 20 2>/dev/null | grep -F "$pod" || true)"
    [ -n "$ended" ] && break
    sleep 2
done
[ -n "$ended" ] || die "the end of the pod's refusals was not recorded"
! grep -qF "refused again and again" <<<"$(owner status)" || die "dispatch status still shows the pod as refused"
step "when the client stops, that is recorded, and dispatch status no longer shows it"
echo "a session that does not hear is noticed and put right, in the trial $name. Take it down with: scripts/trial.sh $context down"
