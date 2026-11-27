#!/usr/bin/env bash
# The task-stream benchmark (ADR 0069): works one arm
# through a stream of tasks, each a fresh session of a trial's agent, in a
# trial installation of the arm's own, and scores it by the world.
#
#   scripts/stream.sh CONTEXT run ARM [--world pilot] [--model MODEL] [--keep]
#   scripts/stream.sh CONTEXT report DIR...     the results of runs, side by side
#
# ARM: none (no memory), notes (cmem with its hook, filled by the sessions),
# tidied (notes, and the dreamer's routines between one day and the next).
# It calls a model: a task is ten to fifteen requests. The model is the
# cheapest unless --model says otherwise, and trials may use only what the
# installation lets them (ADR 0065).
#
# Needs TRIAL_MODEL (the installation's listener for trials) and
# TRIAL_AGENT_BASE (an image with the harness), as scripts/trial.sh does for
# agents that reach the model. Results go to $CARMAN_BENCH_ROOT/stream/
# (~/carman-bench unless set): the world's state, each task's result, the
# memory service's log and the trial's log of runs, and the library as it
# ended. The trial is taken down at the end unless --keep.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "stream: $*" >&2; exit 1; }
context="${1:-}" action="${2:-}"
[ -n "$context" ] && [ -n "$action" ] || die "say: scripts/stream.sh CONTEXT run ARM, or report DIR..."

if [ "$action" = report ]; then
    shift 2
    exec python3 bench/stream/report.py "$@"
fi

[ "$action" = run ] || die "say: run ARM, or report DIR..."
arm="${3:-}"
case "$arm" in none | notes | tidied) ;; *) die "an arm is none, notes or tidied" ;; esac
world=pilot model=claude-haiku-5-5 keep=""
shift 3
while [ $# -gt 0 ]; do
    case "$1" in
    --world) world="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --keep) keep=1; shift ;;
    *) die "unknown option $1" ;;
    esac
done
[ -n "${TRIAL_MODEL:-}" ] && [ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_MODEL, the installation's listener for trials, and TRIAL_AGENT_BASE, an image with the harness"
tasks="bench/stream/$world.json"
[ -f "$tasks" ] || die "no stream of tasks at $tasks"

export TRIAL_NAME="stream-$arm"
name="$TRIAL_NAME" pre="stream-$arm-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
out="${CARMAN_BENCH_ROOT:-$HOME/carman-bench}/stream/$(date -u +%Y%m%d-%H%M%S)-$world-$arm"
mkdir -p "$out"
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
say() { echo "stream[$arm]: $*"; }

# The arm's agent: the same in every arm but for memory. It is held to the
# world's commands (ADR 0061), and to the memory command where the arm has
# memory; with none, the hook that shows memory is off too.
memory=false hook=off
[ "$arm" = none ] || { memory=true; hook=on; }
values="$(mktemp)"
cat > "$values" <<YAML
agents:
  - name: worker
    takesMessages: true
    tools:
      memory: $memory
      run: [ops help, ops stages, ops status, ops owner, ops service, ops change, ops registry, ops deploy, ops config, ops attach]
    models: [$model]
    budgetUSD: 0.5
    deadlineSeconds: 600
    env:
      WORLD_URL: http://${pre}world:8080
      CMEM_HOOK: "$hook"
dreamer:
  models: [$model]
dispatcher:
  maxRunning: 1
YAML
TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"

# The world: one more pod of the trial, an image that is this program and
# nothing else. Its admin word is the runner's and the world's own.
registry="${TRIAL_REGISTRY:-localhost:5050}"
build="$(mktemp -d)"
arch="$(kubectl --context "$context" get --raw /version 2>/dev/null | sed -n 's/.*"platform": *"linux\/\([a-z0-9]*\)".*/\1/p')"
[ -n "$arch" ] || arch="$(uname -m | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -o "$build/stream" ./bench/stream
printf 'FROM scratch\nCOPY stream /stream\nENTRYPOINT ["/stream"]\n' > "$build/Dockerfile"
image="$registry/$name-world:$(shasum -a 256 "$build/stream" | cut -c1-12)"
buildctl build --progress plain --frontend dockerfile.v0 --local context="$build" --local dockerfile="$build" \
    --opt platform="linux/$arch" --output "type=image,name=$image,push=true" > "$build/log" 2>&1 || { tail -20 "$build/log" >&2; die "the world's image did not build"; }
rm -rf "$build"
admin="$(openssl rand -hex 12)"
"${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}world
  labels: {trial: "$name", app: "${pre}world"}
spec:
  containers:
    - name: world
      image: $image
      args: ["world"]
      env:
        - {name: WORLD_NAME, value: "$world"}
        - {name: WORLD_ADMIN, value: "$admin"}
---
apiVersion: v1
kind: Service
metadata:
  name: ${pre}world
  labels: {trial: "$name"}
spec:
  selector: {app: "${pre}world"}
  ports: [{port: 8080}]
YAML
# The agent's command for the world, on its path as an installation's own
# commands are.
"${kube[@]}" create configmap "${pre}tools" --from-file=ops=bench/stream/ops --dry-run=client -o json | "${kube[@]}" apply -f - >/dev/null
"${kube[@]}" label --overwrite "configmap/${pre}tools" "trial=$name" >/dev/null
"${kube[@]}" wait --for=condition=ready "pod/${pre}world" --timeout=120s >/dev/null
world_do() { "${kube[@]}" exec "${pre}world" -- /stream admin "$@"; }
say "the trial $name is up, with the world $world"

# Notes that are in memory from the start, in an arm that has one: an
# agent's observations that nothing has checked, some of them wrong.
if [ "$arm" != none ] && [ -f "bench/stream/$world.seeds.json" ]; then
    token="$("${kube[@]}" get secret "${pre}cmem-tokens" -o 'jsonpath={.data.worker}' | base64 -d)"
    python3 -c 'import json,sys; [print(s["kind"] + "\t" + s["text"]) for s in json.load(open(sys.argv[1]))]' "bench/stream/$world.seeds.json" |
        while IFS="$(printf '\t')" read -r kind text; do
            "${kube[@]}" exec "${pre}cmem-0" -- env CMEM_URL=http://127.0.0.1:8377 "CMEM_TOKEN=$token" \
                cmem note --kind "$kind" --global --evidence "seen once, some time ago, by an agent of the team; nothing checked it" "$text" >/dev/null </dev/null
        done
    say "memory starts with $(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "bench/stream/$world.seeds.json") notes nobody checked"
fi

# One frame of words for every arm; only the sentence about memory differs.
case "$arm" in
none) about="You have no memory of earlier tickets: nothing is carried from one session to the next." ;;
*) about="You have a long-term memory that is carried from one session to the next (the cmem command, and what it showed you as this session began): use what it holds, and before you stop, note in it what this ticket taught that a later one could use." ;;
esac
frame="You work the tickets of a small delivery team, one ticket in each session. The team's systems are reached only with the ops command; ops help lists what it does. $about Do what the ticket asks, then stop, and say in one line what you did."

# Waits for a task of the trial's dispatcher to end, and says how.
ended() {  # TASK SECONDS
    local state="" waited=0
    while [ "$waited" -lt "$2" ]; do
        state="$(owner status "$1" 2>/dev/null | head -1 || true)"
        case "$state" in *": done"*) echo done; return 0 ;; *": failed"*) echo failed; return 0 ;; esac
        sleep 4
        waited=$((waited + 8))
    done
    echo "not ended: ${state:-no word}"
}

python3 - "$tasks" > "$out/order" <<'PY'
import json, sys
for t in json.load(open(sys.argv[1])):
    print(t["id"], t["day"], json.dumps(t["ticket"]))
PY
day=0 began_all="$(date -u +%s)"
: > "$out/tasks.jsonl"
while read -r id tday ticket_json; do
    ticket="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]))' "$ticket_json")"
    if [ "$day" != 0 ] && [ "$tday" != "$day" ] && [ "$arm" = tidied ]; then
        # The night between two days: the dreamer's routines, on this memory.
        night="night-$day-$(date -u +%H%M%S)"
        owner submit "${pre}dreamer" "$night" >/dev/null
        say "night after day $day: $(ended "$night" 1200)"
    fi
    day="$tday"
    world_do task "$id" "$day"
    began="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    sent="$(owner send worker "$frame

Ticket: $ticket")"
    task="message/${sent%% *}"
    how="$(ended "$task" 900)"
    # What the memory service gave and took in that time, and the run's own count.
    "${kube[@]}" logs "${pre}cmem-0" --since-time="$began" 2>/dev/null | grep -E ' msg=(index|recall|read|note|flashback) ' > "$out/$id.memory.log" || true
    usage="$(owner log --type run.usage --last 5 2>/dev/null | grep -F "${sent%% *}" | tail -1 || true)"
    python3 - "$id" "$how" "$usage" >> "$out/tasks.jsonl" <<'PY'
import json, re, sys
tid, how, usage = sys.argv[1:4]
row = {"task": tid, "ended": how}
m = re.search(r'\{.*\}', usage)
if m:
    try:
        u = json.loads(m.group(0))
        row["cost_usd"] = u.get("cost_usd", 0)
        row["tokens"] = u.get("tokens", {})
    except ValueError:
        pass
print(json.dumps(row))
PY
    say "$id (day $day): $how; $(tail -1 "$out/tasks.jsonl" | cut -c1-200)"
done < "$out/order"

world_do state > "$out/state.json"
go run ./bench/stream score "$tasks" "$out/state.json" > "$out/score.json"
# The memory as it ended, and the trial's own record of its runs.
"${kube[@]}" exec "${pre}cmem-0" -- tar -C /data -cf - library > "$out/library.tar" 2>/dev/null || true
mkdir -p "$out/memory" && tar -C "$out/memory" -xf "$out/library.tar" 2>/dev/null || true
owner log --type run.usage --last 100 > "$out/run-usage.log" 2>/dev/null || true
python3 bench/stream/collect.py "$out" "$arm" "$world" "$model" "$began_all" "$tasks"
owner log --last 400 > "$out/dispatcher.log" 2>/dev/null || true
"${kube[@]}" logs "${pre}cmem-0" > "$out/memory.log" 2>/dev/null || true
[ -n "$keep" ] || scripts/trial.sh "$context" down | tail -1
say "results in $out"
scripts/stream.sh "$context" report "$out"
