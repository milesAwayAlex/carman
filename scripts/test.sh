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
# agent-open --reopen (ADR 0053), against a fake tmux: a name not
# remembered is refused, a window that is gone is made, the shell a dead
# harness left is replaced, and a window that runs something else is not.
fake="$(mktemp -d)"
printf '%s\t%s\n' vpn "$fake" > "$fake/list"
cat > "$fake/tmux" <<'SH'
#!/bin/sh
shift 2
echo "$*" >> "$FAKE/calls"
case "$1" in
list-windows) printf '%s' "$FAKE_WINDOWS" ;;
display-message) echo "$FAKE_CMD" ;;
esac
SH
chmod +x "$fake/tmux"
reopen() { FAKE="$fake" AGENT_TMUX="$fake/tmux" AGENT_SESSIONS_FILE="$fake/list" FAKE_WINDOWS="$1" FAKE_CMD="$2" sh deploy/agent/agent-open --reopen "$3" >/dev/null 2>&1; echo $?; }
: > "$fake/calls"
[ "$(reopen '' '' old)" = 3 ] || { echo "agent-open --reopen opens a name it does not remember" >&2; exit 1; }
[ "$(reopen '' '' vpn)" = 0 ] && grep -q '^new-window .* -n vpn ' "$fake/calls" || { echo "agent-open --reopen does not make the window that is gone" >&2; exit 1; }
: > "$fake/calls"
[ "$(reopen vpn bash vpn)" = 0 ] && grep -q '^respawn-window -k -t main:=vpn ' "$fake/calls" || { echo "agent-open --reopen does not replace the shell a dead harness left" >&2; exit 1; }
: > "$fake/calls"
[ "$(reopen vpn claude vpn)" = 4 ] && ! grep -q -e respawn -e new-window "$fake/calls" || { echo "agent-open --reopen opens over a window that runs something" >&2; exit 1; }
rm -rf "$fake"
go vet ./...
go test ./... "$@"
