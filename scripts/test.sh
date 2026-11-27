#!/usr/bin/env bash
# Runs the format check, vet and the tests. Extra arguments go to `go test`,
# for example: scripts/test.sh -run M2 -v
set -euo pipefail

cd "$(dirname "$0")/.."

unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
    echo "These files need gofmt:" >&2
    echo "$unformatted" >&2
    exit 1
fi
# The chart and the agent image are deployed under names an installation
# chooses (scripts/chart-export.sh): only Chart.yaml may say this project's.
named="$(grep -rli carman charts/carman/templates charts/carman/values.yaml deploy/agent deploy/dispatcher deploy/Dockerfile || true)"
if [ -n "$named" ]; then
    echo "These name the project, and what runs in a cluster must not:" >&2
    echo "$named" >&2
    exit 1
fi
# The agent image is built from a list of files in two places, the deploy
# script and the images workflow: what the Dockerfile copies must be in both.
for file in $(sed -n 's/^COPY \(.*\) [^ ]*$/\1/p' deploy/agent/Dockerfile | tr ' ' '\n' | grep -v -e '^cmem-' -e '^dispatcher-' -e '^routines$'); do
    for list in scripts/deploy.sh .github/workflows/images.yml; do
        grep -q -E "deploy/agent/$file( |\$)" "$list" || { echo "$list does not copy deploy/agent/$file, which the agent image needs" >&2; exit 1; }
    done
done
# A new home starts the harness at its prompt: the first-start screens seen,
# the directory agent-open opens trusted, and nothing the home had lost.
if python3 -c "" 2>/dev/null; then  # one that runs: a stub that only names itself does not
    home="$(mktemp -d)"
    echo '{"numStartups":3}' > "$home/.claude.json"
    HOME="$home" sh -c '. deploy/agent/agent-env && agent_harness_state "$HOME"' >/dev/null
    python3 -c 'import json,sys; s=json.load(open(sys.argv[1]+"/.claude.json")); sys.exit(not (s["numStartups"] == 3 and s["hasCompletedOnboarding"] and s["projects"][sys.argv[1]]["hasTrustDialogAccepted"]))' "$(cd "$home" && pwd -P)" ||
        { echo "agent_harness_state does not seed the harness's first start" >&2; rm -rf "$home"; exit 1; }
    rm -rf "$home"
fi
# The idle listener's wake reads as a message, not a hook's error, wherever its settings are.
for f in deploy/agent/settings.json adapters/claude-code/dispatcher.settings.example.json nix/packages/dispatcher-claude-code-settings.nix; do
    [ "$(grep -c rewakeMessage "$f")" -eq "$(grep -c asyncRewake "$f")" ] || { echo "$f has an asyncRewake hook with no rewakeMessage" >&2; exit 1; }
done
# The chart hands the dispatcher who may write to each agent (ADR 0047), and
# refuses a list that names an agent there is not. Checked where helm is.
if command -v helm >/dev/null; then
    render=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0
        --set dispatcher.image=d:0 --set backup.repository=r --show-only templates/dispatcher.yaml)
    "${render[@]}" --set-json 'agents=[{"name":"a","routines":["x"]},{"name":"b","routines":["x"],"writtenBy":["a"]}]' |
        grep -F 'value: "{\"b\":[\"a\"]}"' >/dev/null || { echo "the chart does not hand the dispatcher agent b's writtenBy" >&2; exit 1; }
    if "${render[@]}" --set-json 'agents=[{"name":"b","routines":["x"],"writtenBy":["nobody"]}]' >/dev/null 2>&1; then
        echo "the chart renders a writtenBy that names no agent" >&2
        exit 1
    fi
    # R7: the usage reading reads every model the definitions name, the
    # reading's own every hour, and a value adds or removes one.
    "${render[@]/templates\/dispatcher.yaml/templates/dispatched.yaml}" \
        --set-json 'agents=[{"name":"a","routines":["x"],"models":["m-a"]},{"name":"d","interactive":true,"model":"m-d"}]' \
        --set-json 'dreamer.models=["m-a","m-b"]' --set-json 'dreamer.modelsFor={"x":["m-c"]}' \
        --set usage.model=m-0 --set-json 'usage.addModels=["m-e"]' --set-json 'usage.skipModels=["m-b"]' --set-json 'usage.everyHoursFor={"m-c":2}' |
        grep -F 'value: "m-0=1,m-a=6,m-d=6,m-c=2,m-e=6"' >/dev/null || { echo "the chart does not hand the usage reading the installation's models" >&2; exit 1; }
    # R2: the dreamer, the backup and the warm job are started by the
    # dispatcher where their values say, their CronJobs suspended (ADR 0059).
    nightly=(helm template t charts/carman --set memory.image=m:0 --set agent.image=r.example/a:0 --set dispatcher.image=d:0
        --set backup.enabled=true --set backup.repository=r --set agent.warm.enabled=true)
    out="$("${nightly[@]}" --set dreamer.dispatched=true --set dreamer.suspend=false --set backup.dispatched=true --set agent.warm.dispatched=true)"
    for name in dreamer backup agent-warm; do
        grep -F "\\\"name\\\":\\\"$name\\\"" <<<"$out" >/dev/null || { echo "the dispatcher is not given $name's schedule" >&2; exit 1; }
        awk -v n="$name" '$1 == "kind:" {cj = ($2 == "CronJob")} cj && $1 == "name:" && $2 == n {mine = 1} mine && $1 == "suspend:" {print $2; exit}' <<<"$out" | grep -qx true ||
            { echo "$name's CronJob is not suspended where the dispatcher starts it" >&2; exit 1; }
    done
    ! "${nightly[@]}" --set dreamer.dispatched=true --set dreamer.suspend=true | grep -F '\"name\":\"dreamer\"' >/dev/null ||
        { echo "the dispatcher is given the schedule of a dreamer switched off" >&2; exit 1; }
    ! "${nightly[@]}" | grep -F '\"name\":\"backup\"' >/dev/null || { echo "the dispatcher schedules a backup nobody moved to it" >&2; exit 1; }
    # The owner's machine (ADR 0049): its writtenBy, a name of its own, and
    # a way in from outside only where the addresses are given.
    "${render[@]}" --set-json 'agents=[{"name":"a","routines":["x"]}]' --set-json 'dispatcher.host.writtenBy=["a"]' |
        grep -F 'value: "{\"host\":[\"a\"]}"' >/dev/null || { echo "the chart does not hand the dispatcher the host's writtenBy" >&2; exit 1; }
    if "${render[@]}" --set-json 'agents=[{"name":"host","routines":["x"]}]' >/dev/null 2>&1; then
        echo "the chart renders a place for the owner's machine named for an agent" >&2
        exit 1
    fi
    if "${render[@]}" --set dispatcher.service.type=LoadBalancer >/dev/null 2>&1; then
        echo "the chart renders an isolated dispatcher reached from outside with no addresses to admit" >&2
        exit 1
    fi
    "${render[@]}" --set dispatcher.service.type=LoadBalancer --set-json 'dispatcher.service.admitFrom=["10.0.0.0/8"]' |
        grep -F 'cidr: "10.0.0.0/8"' >/dev/null || { echo "the chart does not admit the owner's machine to the dispatcher" >&2; exit 1; }
    # R2: no pod is given the owner's token, and the dispatcher gets its hash (ADR 0051).
    whole="$(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r \
        --set-json 'agents=[{"name":"a","routines":["x"],"interactive":true}]')"
    if grep -qE '^ *key: owner$' <<<"$whole"; then
        echo "the chart gives a pod the owner's token" >&2
        exit 1
    fi
    grep -qE '^ *key: owner-sha256$' <<<"$whole" || { echo "the chart does not give the dispatcher the owner's token's hash" >&2; exit 1; }
    # A trial installation's dispatcher (ADR 0054): an account in the sandbox
    # only when asked for, and the trial's values render.
    sandbox=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0
        --set agent.sandbox.namespace=sb --set-json 'agents=[{"name":"a","routines":["x"]}]' --show-only templates/sandbox.yaml)
    ! "${sandbox[@]}" | grep -F 'name: trial-dispatcher' >/dev/null || { echo "the sandbox has a trial account nobody asked for" >&2; exit 1; }
    "${sandbox[@]}" --set agent.sandbox.trial=true --set-json 'agent.sandbox.api.addresses=["192.0.2.1/32"]' |
        grep -F 'name: trial-reaches-the-api' >/dev/null || { echo "the sandbox's trial account cannot reach the API" >&2; exit 1; }
    # An agent has the installation's definition only where its entry says so (ADR 0056).
    agents=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --show-only templates/agents.yaml)
    ! "${agents[@]}" --set-json 'agents=[{"name":"a","interactive":true}]' | grep -F 'mountPath: /etc/installation' >/dev/null ||
        { echo "an agent is given the installation's definition without being asked to" >&2; exit 1; }
    "${agents[@]}" --set-json 'agents=[{"name":"a","interactive":true,"definition":true}]' | grep -F 'mountPath: /etc/installation' >/dev/null ||
        { echo "an agent with definition: true does not get the installation's definition" >&2; exit 1; }
    helm template t charts/carman -f deploy/values/trial.yaml --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 >/dev/null ||
        { echo "the trial's values do not render" >&2; exit 1; }
else
    echo "helm is not here: the chart's checks are skipped" >&2
fi
# The nightly backup keeps agents' sessions and kept conversations in a form
# that comes back whole (ADR 0038, RECOVERY.md). Checked where restic is.
scripts/check-backup-copy.sh
# Nothing here needs a C compiler, and not every place that runs this has one.
export CGO_ENABLED="${CGO_ENABLED:-0}"
# The installation's definition from within a pod (ADR 0056), with no
# cluster: an agent's clone of the bundle, its proposal as a message's text,
# and the owner's side applying it as the same commit, or refusing it whole.
defn="$(mktemp -d)"
(
    set -e
    quiet() { "$@" >/dev/null 2>&1; }
    export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
    unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
    mkdir -p "$defn/d/tools" && cd "$defn/d" && git init -q -b main && git config user.name owner && git config user.email o@example.invalid
    printf 'agents: []\n' > values.yaml && printf '#!/bin/sh\n' > tools/hello && printf 'dispatcher.env\n' > .gitignore
    git add -A && git commit -q -m "The first definition"
    cd "$OLDPWD"
    propose() {  # NAME, then what changes the clone
        git -C "$defn/d" bundle create -q "$defn/b" HEAD --branches
        rm -rf "$defn/c"
        AGENT_DEFINITION_BUNDLE="$defn/b" AGENT_NAME=desk sh deploy/agent/agent-definition clone "$defn/c" >/dev/null
        # Written as somebody else, as a pod's environment makes git do.
        (cd "$defn/c" && eval "$2" && git add -A && GIT_AUTHOR_NAME=Somebody GIT_AUTHOR_EMAIL=s@example.invalid git commit -q -m "$1")
        AGENT_DEFINITION_BUNDLE="$defn/b" AGENT_NAME=desk sh deploy/agent/agent-definition propose --print "$defn/c" > "$defn/message"
    }
    apply() { CARMAN_INSTALLATION="$defn/d" scripts/deploy.sh none definition apply --file "$defn/message"; }
    propose "An agent is added" 'printf "agents:\n  - name: desk\n" > values.yaml'
    head -1 "$defn/message" | grep -q "^A proposed change to the installation's definition: An agent is added$" || { echo "a proposal's first line does not say what it is" >&2; exit 1; }
    quiet apply || { echo "a proposal that applies is refused" >&2; exit 1; }
    [ "$(git -C "$defn/d" log -1 --format='%an|%s')" = "agent desk|An agent is added" ] && git -C "$defn/d" log -1 --format=%B | grep -q '^Proposed-in: ' ||
        { echo "an applied proposal does not keep its author and message, with where it came from" >&2; exit 1; }
    # The clone, brought up to date, has nothing of its own left: its commit is deployed.
    git -C "$defn/d" bundle create -q "$defn/b" HEAD --branches
    AGENT_DEFINITION_BUNDLE="$defn/b" sh deploy/agent/agent-definition update "$defn/c" | grep -q "0 commit(s) of yours" || { echo "a clone is not brought up to date" >&2; exit 1; }
    for refused in "git's own:echo x >> .gitignore" "this machine's:echo t > dispatcher.env; git add -f dispatcher.env" "a link:ln -s /etc/passwd tools/link" "a new top-level:echo x > bench.env" "another new top-level:echo x > notes"; do
        before="$(git -C "$defn/d" rev-parse HEAD)"
        propose "${refused%%:*}" "${refused#*:}"
        if quiet apply || [ "$(git -C "$defn/d" rev-parse HEAD)" != "$before" ] || [ -n "$(git -C "$defn/d" status --porcelain)" ]; then
            echo "a proposal that touches ${refused%%:*} file is applied, or leaves something behind" >&2; exit 1
        fi
    done
    propose "A new routine" 'mkdir -p routines && echo "Do it." > routines/new.md'
    quiet apply && [ -f "$defn/d/routines/new.md" ] || { echo "a proposal that adds a routine is refused" >&2; exit 1; }
    # Made against a definition that has moved on where it changes: refused whole.
    propose "Stale" 'echo "echo mine" >> tools/hello'
    (cd "$defn/d" && echo "echo theirs" >> tools/hello && git commit -q -am "Newer")
    before="$(git -C "$defn/d" rev-parse HEAD)"
    if quiet apply || [ "$(git -C "$defn/d" rev-parse HEAD)" != "$before" ] || [ "$(git -C "$defn/d" worktree list | wc -l)" -ne 1 ]; then
        echo "a proposal against an older definition is applied, or leaves something behind" >&2; exit 1
    fi
) || { rm -rf "$defn"; exit 1; }
rm -rf "$defn"
# agent-open --reopen (ADR 0053), against a fake tmux and real processes: a
# name not remembered is refused, a window that is gone is made, the shell a
# dead harness left is replaced, and a window that runs a harness, a shell
# with a job, or is in front of a person attached, is not.
fake="$(mktemp -d)"
printf '%s\t%s\n' vpn "$fake" > "$fake/list"
cat > "$fake/tmux" <<'SH'
#!/bin/sh
shift 2
echo "$*" >> "$FAKE/calls"
case "$1" in
list-windows) printf '%s' "$FAKE_WINDOWS" ;;
display-message)
    case "$*" in
    *pane_pid*) echo "$FAKE_PID" ;;
    *session_attached*) echo "${FAKE_ATTACHED:-0}" ;;
    esac ;;
esac
SH
printf '#!/bin/sh\nsleep 60\n' > "$fake/claude"
chmod +x "$fake/tmux" "$fake/claude"
mkfifo "$fake/fifo"
bash -c "read -r x < '$fake/fifo'" & idle=$!
sh -c 'sleep 60; true' & job=$!
"$fake/claude" & harness=$!
sleep 0.3
reopen() { FAKE="$fake" AGENT_TMUX="$fake/tmux" AGENT_SESSIONS_FILE="$fake/list" FAKE_WINDOWS="$1" FAKE_PID="$2" FAKE_ATTACHED="${4:-0}" sh deploy/agent/agent-open --reopen "$3" >/dev/null 2>&1; echo $?; }
fail() { kill "$idle" "$job" "$harness" 2>/dev/null; rm -rf "$fake"; echo "$1" >&2; exit 1; }
: > "$fake/calls"
[ "$(reopen '' '' old)" = 3 ] || fail "agent-open --reopen opens a name it does not remember"
[ "$(reopen '' '' vpn)" = 0 ] && grep -q '^new-window .* -n vpn ' "$fake/calls" || fail "agent-open --reopen does not make the window that is gone"
# What a window runs is read from /proc, as in a pod: checked where there is one.
if [ -r "/proc/$$/comm" ]; then
: > "$fake/calls"
[ "$(reopen vpn "$idle" vpn)" = 0 ] && grep -q '^respawn-window -k -t main:=vpn ' "$fake/calls" || fail "agent-open --reopen does not replace the shell a dead harness left"
for case in "$harness:a live harness" "$job:a shell with a job" "$idle 1:a window a person attached is looking at"; do
    : > "$fake/calls"
    what="${case%%:*}"
    [ "$(reopen vpn "${what% *}" vpn "$([ "${what#* }" = 1 ] && echo 1 || echo 0)")" = 4 ] && ! grep -q -e respawn -e new-window "$fake/calls" || fail "agent-open --reopen opens over ${case#*:}"
done
fi
kill "$idle" "$job" "$harness" 2>/dev/null || true
rm -rf "$fake"
go vet ./...
go test ./... "$@"
