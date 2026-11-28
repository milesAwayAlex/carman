#!/usr/bin/env bash
# The task-stream benchmark (ADR 0069): works one arm
# through a stream of tasks, each a fresh session of a trial's agent, in a
# trial installation of the arm's own, and scores it by the world.
#
#   scripts/stream.sh CONTEXT run ARM [--world pilot] [--model MODEL] [--keep]
#   scripts/stream.sh CONTEXT run ARM --resume DIR    go on with a run that stopped, at the
#                                               task it stopped at, in its trial, which is still up
#   scripts/stream.sh CONTEXT report DIR...     the results of runs, side by side
#   scripts/stream.sh CONTEXT all [--world W] [--model M] [--times 3] [--budget USD]
#                                               every arm, so many times each, two arms at a time;
#                                               stops when the runs' own count of cost passes the budget
#
# ARM: none (no memory), builtin (the harness's own memory, in a directory
# kept from task to task), notes (cmem with its hook, filled by the
# sessions), tidied (notes, and the dreamer's routines between one day and
# the next).
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
#
# When a task's run stops at the plan's usage limit (the dispatcher says so,
# with when the limit resets, ADR 0067), the runner waits for the reset and
# gives the same ticket to a fresh session, as often as it takes. When a
# session uses nothing of the model for another reason (the installation's
# limit for trials), or a night fails, it stops with status 3 and leaves
# the trial up and its place in DIR: --resume DIR goes on from there. A
# task that was given more than once says so in its result (attempts).
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "stream: $*" >&2; exit 1; }
context="${1:-}" action="${2:-}"
[ -n "$context" ] && [ -n "$action" ] || die "say: scripts/stream.sh CONTEXT run ARM, or report DIR..."

if [ "$action" = report ]; then
    shift 2
    exec python3 bench/stream/report.py "$@"
fi

if [ "$action" = all ]; then
    shift 2
    exec python3 bench/stream/all.py "$context" "$@"
fi

[ "$action" = run ] || die "say: run ARM, all, or report DIR..."
arm="${3:-}"
case "$arm" in none | builtin | notes | tidied) ;; *) die "an arm is none, builtin, notes or tidied" ;; esac
world=pilot model=claude-haiku-5-5 keep="" resume="" stop_after=""
shift 3
while [ $# -gt 0 ]; do
    case "$1" in
    --world) world="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --keep) keep=1; shift ;;
    --resume) resume="$2"; shift 2 ;;
    # For trying the resume: stop after this task as if the model had been refused.
    --stop-after) stop_after="$2"; shift 2 ;;
    *) die "unknown option $1" ;;
    esac
done
[ -n "${TRIAL_MODEL:-}" ] && [ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_MODEL, the installation's listener for trials, and TRIAL_AGENT_BASE, an image with the harness"
if [ -n "$resume" ]; then
    [ -f "$resume/resume.env" ] || die "$resume is not a run that stopped: it has no resume.env"
    # shellcheck disable=SC1091
    . "$resume/resume.env"
    [ "$resume_arm" = "$arm" ] || die "$resume is a run of the arm $resume_arm, not $arm"
    world="$resume_world" model="$resume_model"
fi
tasks="bench/stream/$world.json"
[ -f "$tasks" ] || die "no stream of tasks at $tasks"

export TRIAL_NAME="stream-$arm"
name="$TRIAL_NAME" pre="stream-$arm-"
namespace="${TRIAL_NAMESPACE:-$(kubectl config view --minify --context "$context" -o 'jsonpath={..namespace}')}"
kube=(kubectl --context "$context" --namespace "$namespace")
out="${resume:-${CARMAN_BENCH_ROOT:-$HOME/carman-bench}/stream/$(date -u +%Y%m%d-%H%M%S)-$world-$arm}"
mkdir -p "$out"
owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
say() { echo "stream[$arm]: $*"; }

world_do() { "${kube[@]}" exec "${pre}world" -- /stream admin "$@"; }
if [ -z "$resume" ]; then
# The arm's agent: the same in every arm but for memory. It is held to the
# world's commands (ADR 0061), and to the memory command where the arm has
# memory; with none, the hook that shows memory is off too.
memory=false hook=off builtin="" storage=""
case "$arm" in
notes | tidied) memory=true hook=on ;;
builtin)
    # The harness's own memory: on, in one directory on the volume that
    # outlives a task's pod, which the agent may write under and nowhere else.
    builtin="
      AGENT_BUILTIN_MEMORY: /var/lib/agent/conversations/builtin-memory
      CMEM_ROUTINE_WRITE: /var/lib/agent/conversations/builtin-memory"
    storage="
agent:
  sessionStorage: 1Gi" ;;
esac
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
      CMEM_HOOK: "$hook"$builtin$storage
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
say "the trial $name is up, with the world $world"

# Notes that are in memory from the start, in an arm that has one: an
# agent's observations that nothing has checked, some of them wrong.
if [ "$arm" = builtin ] && [ -f "bench/stream/$world.seeds.json" ]; then
    # The same notes, as the harness's own memory keeps them: a file each,
    # and a line for each in MEMORY.md. Written by a pod that has the volume.
    seed="$(python3 - "bench/stream/$world.seeds.json" <<'PY'
import json, sys
index, files = [], []
for n, s in enumerate(json.load(open(sys.argv[1])), 1):
    name = "seen-earlier-%d.md" % n
    index.append("- [Seen earlier, unchecked %d](%s) — %s" % (n, name, s["text"]))
    files.append("cat > %s <<'EOF'\n---\nname: seen-earlier-%d\ndescription: an agent's observation that nothing has checked\n---\n\n%s\n\nSeen once, some time ago, by an agent of the team; nothing checked it.\nEOF" % (name, n, s["text"]))
print("mkdir -p /m/builtin-memory && cd /m/builtin-memory\n" + "\n".join(files) + "\ncat > MEMORY.md <<'EOF'\n" + "\n".join(index) + "\nEOF")
PY
)"
    dimage="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"
    "${kube[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pre}seed
  labels: {trial: "$name"}
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: $dimage
      command: ["sleep", "300"]
      volumeMounts: [{name: sessions, mountPath: /m, subPath: worker}]
  volumes:
    - name: sessions
      persistentVolumeClaim: {claimName: ${pre}agent-sessions}
YAML
    "${kube[@]}" wait --for=condition=ready "pod/${pre}seed" --timeout=120s >/dev/null
    "${kube[@]}" exec -i "${pre}seed" -- sh -s <<<"$seed"
    "${kube[@]}" delete pod "${pre}seed" --wait=false >/dev/null
    say "the harness's own memory starts with the notes nobody checked"
elif [ "$arm" != none ] && [ -f "bench/stream/$world.seeds.json" ]; then
    token="$("${kube[@]}" get secret "${pre}cmem-tokens" -o 'jsonpath={.data.worker}' | base64 -d)"
    python3 -c 'import json,sys; [print(s["kind"] + "\t" + s["text"]) for s in json.load(open(sys.argv[1]))]' "bench/stream/$world.seeds.json" |
        while IFS="$(printf '\t')" read -r kind text; do
            "${kube[@]}" exec "${pre}cmem-0" -- env CMEM_URL=http://127.0.0.1:8377 "CMEM_TOKEN=$token" \
                cmem note --kind "$kind" --global --evidence "seen once, some time ago, by an agent of the team; nothing checked it" "$text" >/dev/null </dev/null
        done
    say "memory starts with $(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "bench/stream/$world.seeds.json") notes nobody checked"
fi

else
    "${kube[@]}" get pod "${pre}world" >/dev/null 2>&1 || die "the trial $name of the run in $resume is not up any more: the run cannot go on"
    say "going on with the run in $resume, at $resume_next"
fi

# One frame of words for every arm; only the sentence about memory differs.
case "$arm" in
none) about="You have no memory of earlier tickets: nothing is carried from one session to the next." ;;
builtin) about="You have a long-term memory that is carried from one session to the next (your memory directory, and what it showed you as this session began): use what it holds, and before you stop, save in it what this ticket taught that a later one could use." ;;
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
day=0 began_all="${resume_began:-$(date -u +%s)}"
[ -n "$resume" ] || : > "$out/tasks.jsonl"
touch "$out/nights"
# Stops the run where it is, to be gone on with: the trial stays up.
stop_here() {  # TASK WHY
    printf 'resume_arm=%s\nresume_world=%s\nresume_model=%s\nresume_next=%s\nresume_began=%s\n' "$arm" "$world" "$model" "$1" "$began_all" > "$out/resume.env"
    say "stopped at $1: $2. The trial $name stays up; go on with: scripts/stream.sh $context run $arm --resume $out"
    exit 3
}
skipping="${resume_next:-}"
while read -r id tday ticket_json; do
    if [ -n "$skipping" ]; then
        # Tasks done before the stop: only the day is taken from them.
        [ "$id" = "$skipping" ] || { day="$tday"; continue; }
        skipping=""
    fi
    ticket="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]))' "$ticket_json")"
    if [ "$day" != 0 ] && [ "$tday" != "$day" ] && [ "$arm" = tidied ] && ! grep -qx "$day" "$out/nights"; then
        # The night between two days: the dreamer's routines, on this memory.
        # One that fails is tried again after a while (the plan's limit,
        # most likely), and then the run stops to be gone on with.
        nights=0
        while :; do
            night="night-$day-$(date -u +%H%M%S)"
            owner submit "${pre}dreamer" "$night" >/dev/null
            went="$(ended "$night" 1800)"
            say "night after day $day: $went"
            [ "$went" != done ] || break
            nights=$((nights + 1))
            [ "$nights" -lt 3 ] || stop_here "$id" "the night after day $day failed three times"
            sleep 900
        done
        echo "$day" >> "$out/nights"
    fi
    day="$tday"
    attempts=0
    while :; do
        attempts=$((attempts + 1))
        world_do task "$id" "$day"
        [ "$attempts" -gt 1 ] || began="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        sent="$(owner send worker "$frame

Ticket: $ticket")"
        task="message/${sent%% *}"
        how="$(ended "$task" 900)"
        # Read more than once: one read that fails (the owner's client is a
        # port forwarded each time) once stopped a run whose session had run.
        usage=""
        for _ in 1 2 3 4 5 6; do
            usage="$(owner log --type run.usage --last 8 2>/dev/null | grep -F "${sent%% *}" | tail -1 || true)"
            [ -z "$usage" ] || break
            sleep 5
        done
        # Did the run stop at the plan's usage limit? The dispatcher says so
        # of the message, with when the limit resets.
        limit="$(owner message "${sent%% *}" 2>/dev/null | python3 bench/stream/limit.py || echo none)"
        case "$limit" in
        none) break ;;
        wait-unknown) pause=1800 ;;
        *) pause="${limit#wait }" ;;
        esac
        say "$id: the run stopped at the plan's usage limit; waiting $((pause / 60)) minutes for its reset, then giving the ticket to a fresh session"
        sleep "$pause"
    done
    # What the memory service gave and took in that time, and the run's own count.
    "${kube[@]}" logs "${pre}cmem-0" --since-time="$began" 2>/dev/null | grep -E ' msg=(index|recall|read|note|flashback) ' > "$out/$id.memory.log" || true
    row="$(python3 - "$id" "$how" "$usage" "$attempts" <<'PY'
import json, re, sys
tid, how, usage, attempts = sys.argv[1:5]
row = {"task": tid, "ended": how}
if int(attempts) > 1:
    row["attempts"] = int(attempts)
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
)"
    # A session that used nothing, and was not stopped by the plan's limit,
    # did not run: the model was refused it another way (the installation's
    # limit for trials, most likely). The run stops here, to be gone on with.
    if [ "$stop_after" = "$id" ]; then
        echo "$row" >> "$out/tasks.jsonl"
        next="$(awk -v id="$id" 'found {print $1; exit} $1 == id {found = 1}' "$out/order")"
        [ -z "$next" ] || stop_here "$next" "asked to, after $id (--stop-after)"
    elif grep -q '"cost_usd": 0[,}]' <<<"$row" || ! grep -q cost_usd <<<"$row"; then
        stop_here "$id" "its session used nothing of the model, and the dispatcher does not say the plan's limit stopped it (the installation's limit for trials, egress.model.trials.requests, ADR 0065?)"
    fi
    [ "$stop_after" = "$id" ] || echo "$row" >> "$out/tasks.jsonl"
    say "$id (day $day): $how; $(cut -c1-200 <<<"$row")"
done < "$out/order"

world_do state > "$out/state.json"
go run ./bench/stream score "$tasks" "$out/state.json" > "$out/score.json"
# The memory as it ended, and the trial's own record of its runs.
"${kube[@]}" exec "${pre}cmem-0" -- tar -C /data -cf - library > "$out/library.tar" 2>/dev/null || true
if [ "$arm" = builtin ]; then
    # The harness's own memory as it ended: read through a pod that has the volume.
    "${kube[@]}" run "${pre}read" --restart=Never --labels="trial=$name" --image="$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')" \
        --overrides='{"spec":{"containers":[{"name":"read","image":"'"$("${kube[@]}" get deployment "${pre}dispatcher" -o 'jsonpath={.spec.template.spec.containers[0].image}')"'","command":["sleep","120"],"volumeMounts":[{"name":"s","mountPath":"/m","subPath":"worker"}]}],"volumes":[{"name":"s","persistentVolumeClaim":{"claimName":"'"${pre}agent-sessions"'"}}]}}' >/dev/null 2>&1 || true
    "${kube[@]}" wait --for=condition=ready "pod/${pre}read" --timeout=90s >/dev/null 2>&1 && "${kube[@]}" exec "${pre}read" -- tar -C /m -cf - builtin-memory > "$out/builtin-memory.tar" 2>/dev/null || true
    "${kube[@]}" delete pod "${pre}read" --wait=false >/dev/null 2>&1 || true
fi
mkdir -p "$out/memory" && tar -C "$out/memory" -xf "$out/library.tar" 2>/dev/null || true
owner log --type run.usage --last 100 > "$out/run-usage.log" 2>/dev/null || true
python3 bench/stream/collect.py "$out" "$arm" "$world" "$model" "$began_all" "$tasks"
owner log --last 400 > "$out/dispatcher.log" 2>/dev/null || true
"${kube[@]}" logs "${pre}cmem-0" > "$out/memory.log" 2>/dev/null || true
rm -f "$out/resume.env"
[ -n "$keep" ] || scripts/trial.sh "$context" down | tail -1
say "results in $out"
scripts/stream.sh "$context" report "$out"
