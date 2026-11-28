#!/usr/bin/env bash
# Runs the format check, vet and the tests. Extra arguments go to `go test`,
# for example: scripts/test.sh -run M2 -v
#
# A check that needs what is not here (Postgres, helm, restic, tmux, python3)
# is skipped and named at the end. With --strict first, a skip is a failure:
# for whoever merges. Postgres is DISPATCH_TEST_DATABASE_URL.
set -euo pipefail

cd "$(dirname "$0")/.."
strict=""
[ "${1:-}" != --strict ] || { strict=1; shift; }
skipped=()
skip() { skipped+=("$1"); echo "skipped: $1" >&2; }

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
# Every script a pod of the chart starts with waits, once, until the network
# knows the pod (ADR 0066): by agent_setup, or by dispatcher ready itself.
for first in run-routine usage-reading agent-session agent-resume backup-state warm-harness; do
    grep -q -e 'agent_setup' -e 'dispatcher ready' "deploy/agent/$first" ||
        { echo "deploy/agent/$first is a pod's first command and does not wait for the network to know the pod (agent_setup, or dispatcher ready)" >&2; exit 1; }
done
grep -q 'dispatcher ready' deploy/agent/agent-env || { echo "agent_setup does not wait for the network to know the pod" >&2; exit 1; }
# A new home starts the harness at its prompt: the first-start screens seen,
# the directory agent-open opens trusted, and nothing the home had lost.
if python3 -c "" 2>/dev/null; then  # one that runs: a stub that only names itself does not
    home="$(mktemp -d)"
    echo '{"numStartups":3}' > "$home/.claude.json"
    HOME="$home" sh -c '. deploy/agent/agent-env && agent_harness_state "$HOME"' >/dev/null
    python3 -c 'import json,sys; s=json.load(open(sys.argv[1]+"/.claude.json")); sys.exit(not (s["numStartups"] == 3 and s["hasCompletedOnboarding"] and s["projects"][sys.argv[1]]["hasTrustDialogAccepted"]))' "$(cd "$home" && pwd -P)" ||
        { echo "agent_harness_state does not seed the harness's first start" >&2; rm -rf "$home"; exit 1; }
    rm -rf "$home"
    # A restricted job's harness has only the allow rules cmem routine gives
    # it, not the image's beside them (ADR 0061); one with every tool, and a
    # standing pod, have the image's.
    for case in "memory:0" "named:0" "all:1" "interactive:1"; do
        home="$(mktemp -d)" && mkdir "$home/.claude"
        tools="${case%%:*}" interactive=""
        [ "$tools" != interactive ] || { tools="" interactive=1; }
        HOME="$home" CMEM_ROUTINE_TOOLS="$tools" AGENT_INTERACTIVE="$interactive" AGENT_SETTINGS=deploy/agent/settings.json \
            sh -c '. deploy/agent/agent-env && agent_harness_settings'
        python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit((len(s["permissions"]["allow"]) > 0) != (sys.argv[2] == "1") or not s["hooks"])' \
            "$home/.claude/settings.json" "${case#*:}" || { echo "agent_harness_settings: the image's allow rules are wrong for tools ${case%%:*}" >&2; rm -rf "$home"; exit 1; }
        rm -rf "$home"
    done
else
    skip "the harness's first start and settings (agent_harness_state, agent_harness_settings): no python3"
fi
# The notice job runs the installation's own command, which is on the PATH
# only once the agent's environment is set up (seen 2026-11-27: every notice
# failed with "notify-owner: not found").
grep -q 'agent-env && agent_setup.*exec notify-owner' charts/carman/templates/dispatched.yaml ||
    { echo "the notify job does not set up the agent's environment before notify-owner" >&2; exit 1; }
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
    if "${render[@]}" --set dispatcher.improvement.surveyor=a --set-json 'agents=[{"name":"a","routines":["x"]},{"name":"b","routines":["x"],"writtenBy":["a"]}]' >/dev/null 2>&1; then
        echo "the chart renders a writtenBy that names the improvement list's surveyor (ADR 0058)" >&2
        exit 1
    fi
    if "${render[@]}" --set-json 'agents=[{"name":"b","routines":["x"],"writtenBy":["nobody"]}]' >/dev/null 2>&1; then
        echo "the chart renders a writtenBy that names no agent" >&2
        exit 1
    fi
    # The dreamer is a party to messages where the dispatcher is on: known
    # by its label, its pod's identity mounted, the dispatcher's hooks on;
    # with messages off, or no dispatcher, none of it. Nobody's writtenBy
    # may name it (ADR 0070).
    dreamer=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0
        --set backup.repository=r --show-only templates/dreamer.yaml)
    out="$("${dreamer[@]}" --set dispatcher.enabled=true)"
    for want in 'agent: dreamer' 'name: DISPATCHER_HOOK' 'audience: dispatcher' 'mountPath: /var/run/secrets/dispatcher'; do
        grep -F "$want" <<<"$out" >/dev/null || { echo "the dreamer is no party to messages: no $want" >&2; exit 1; }
    done
    for off in "--set dispatcher.enabled=false" "--set dispatcher.enabled=true --set dreamer.messages=false"; do
        # shellcheck disable=SC2086
        ! "${dreamer[@]}" $off | grep -E 'agent: dreamer|DISPATCHER_HOOK|audience: dispatcher' >/dev/null ||
            { echo "the dreamer is a party to messages with $off" >&2; exit 1; }
    done
    if "${render[@]}" --set-json 'agents=[{"name":"b","routines":["x"],"writtenBy":["dreamer"]}]' >/dev/null 2>&1; then
        echo "the chart renders a writtenBy that names the dreamer" >&2
        exit 1
    fi
    # A host may have two credentials, each on a header of its own (ADR
    # 0031): it is answered once, both are added there, and two for one
    # header are refused.
    two="$(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r \
        --set-json 'egress.credentials=[{"host":"a.example.com","key":"app"},{"host":"a.example.com","key":"front","header":"Proxy-Authorization"},{"host":"b.example.com","key":"b"}]' \
        --set-json 'egress.renewals=[{"key":"app","command":["x"],"also":["front"]}]')"
    # One renewal may write both (also): each is read again from its file, and one container renews.
    [ "$(grep -c 'path: /etc/envoy/renewed/' <<<"$two")" = 2 ] && [ "$(grep -c 'name: renew-' <<<"$two")" = 1 ] && grep -q 'for key in app front;' <<<"$two" ||
        { echo "the chart does not take a renewal's other key (also) as renewed" >&2; exit 1; }
    [ "$(grep -c 'server_listener_name: answer-' <<<"$two")" = 2 ] && grep -q 'header: Proxy-Authorization' <<<"$two" &&
        [ "$(grep -c 'header: Authorization' <<<"$two")" = 2 ] && ! grep -q 'answer-front' <<<"$two" ||
        { echo "the chart does not answer a host once with both of its credentials, each on its header" >&2; exit 1; }
    if helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r \
        --set-json 'egress.credentials=[{"host":"a.example.com","key":"app"},{"host":"a.example.com","key":"c"}]' >/dev/null 2>&1; then
        echo "the chart renders two credentials of one host for one header" >&2; exit 1
    fi
    # Agents' limits at the proxy reach the dispatcher (ADR 0064).
    "${render[@]}" --set-json 'agents=[{"name":"a","routines":["x"],"proxyLimit":{"day":40000000}},{"name":"b","routines":["x"]}]' \
        --set-json 'dreamer.proxyLimit={"pod":8000000}' |
        grep -F 'value: "{\"a\":{\"day\":40000000},\"dreamer\":{\"pod\":8000000}}"' >/dev/null ||
        { echo "the chart does not hand the dispatcher agents' limits at the proxy" >&2; exit 1; }
    # R7: the usage reading reads every model the definitions name, the
    # reading's own every hour, and a value adds or removes one.
    "${render[@]/templates\/dispatcher.yaml/templates/dispatched.yaml}" \
        --set-json 'agents=[{"name":"a","routines":["x"],"models":["m-a"]},{"name":"d","interactive":true,"model":"m-d"}]' \
        --set-json 'dreamer.models=["m-a","m-b"]' --set-json 'dreamer.modelsFor={"x":["m-c"]}' \
        --set usage.model=m-0 --set-json 'usage.addModels=["m-e"]' --set-json 'usage.skipModels=["m-b"]' --set-json 'usage.everyHoursFor={"m-c":2}' |
        grep -F 'value: "m-0=1,m-a=6,m-d=6,m-c=2,m-e=6"' >/dev/null || { echo "the chart does not hand the usage reading the installation's models" >&2; exit 1; }
    # An agent's pod is told the image it runs: a job the warm one, a
    # standing pod agent.image.
    out="$(helm template t charts/carman --set memory.image=m:0 --set agent.image=r.example/a:0 --set dispatcher.image=d:0 \
        --set backup.repository=r --set agent.warm.enabled=true --show-only templates/agents.yaml \
        --set-json 'agents=[{"name":"j","routines":["x"]},{"name":"s","interactive":true}]' | grep -A1 'name: AGENT_IMAGE' | grep -o 'value: .*' | sort -u | tr '\n' ' ')"
    [ "$out" = 'value: "r.example/a:0" value: "r.example/a:warm" ' ] || { echo "agent pods are not told the image they run: $out" >&2; exit 1; }
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
    grep -F '\"name\":\"backup\"' <<<"$out" | grep -F '\"after\":\"dreamer\"' >/dev/null ||
        { echo "the dispatched backup is not set after the dreamer (ADR 0062)" >&2; exit 1; }
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
    # The improvement loop (ADR 0058): with nothing said it sends nothing and
    # tells nobody, and its surveyor is an agent of the installation.
    "${render[@]}" | grep -F 'name: DISPATCHER_IMPROVEMENT' -A1 | grep -F '\"send\":false' | grep -F '\"notify\":false' | grep -F '\"surveyor\":\"\"' >/dev/null ||
        { echo "the chart does not hand the dispatcher an improvement loop that is off" >&2; exit 1; }
    if "${render[@]}" --set dispatcher.improvement.surveyor=nobody >/dev/null 2>&1 || "${render[@]}" --set dispatcher.improvement.send=true >/dev/null 2>&1 ||
        "${render[@]}" --set dispatcher.improvement.usage.rule=room >/dev/null 2>&1 || ! "${render[@]}" --set dispatcher.improvement.usage.rule=room --set dispatcher.pace.room=true >/dev/null 2>&1; then
        echo "the chart renders an improvement loop whose surveyor is no agent, or that sends with none, or that waits for room pacing does not record" >&2
        exit 1
    fi
    # R2, triggers (ADR 0063): none unless written; a watch is a job with no
    # model or memory credential, on the dispatcher's schedule; an agent may
    # not take a watch's job's name; the triggers' trial values render.
    plain="$(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r)"
    ! grep -qE 'DISPATCHER_(RULES|WATCHES|HOOKS)|name: watch-' <<<"$plain" || { echo "the chart renders triggers nobody wrote" >&2; exit 1; }
    watched=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r
        --set-json 'dispatcher.watches=[{"name":"page","cron":"*/30 * * * *","url":"https://example.com/"}]')
    job="$("${watched[@]}" --show-only templates/dispatched.yaml | awk '/name: watch-page$/,0')"
    grep -q 'command: \["dispatcher", "ready", "--", "dispatcher", "source"\]' <<<"$job" && ! grep -qE 'CLAUDE_CODE_OAUTH_TOKEN|CMEM_TOKEN|secretKeyRef' <<<"$job" ||
        { echo "a watch's job is not the watch alone: it has a credential, or does not run the watch" >&2; exit 1; }
    "${watched[@]}" --show-only templates/dispatcher.yaml | grep -F '\"class\":\"watch-page\"' >/dev/null || { echo "the chart does not schedule a watch" >&2; exit 1; }
    if "${watched[@]}" --set-json 'agents=[{"name":"watch-page","routines":["x"]}]' >/dev/null 2>&1; then
        echo "the chart renders an agent named as a watch's job is" >&2
        exit 1
    fi
    # R10, trials reach the model (ADR 0065):
    # only where switched on, with the credential at the proxy and a sandbox;
    # then a listener of the meter's own, the cheapest model unless others
    # are listed, a Role over the sandbox's pods only, and a way out of the
    # sandbox to that port alone.
    metered=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set agent.modelCredential=proxy
        --set agent.sandbox.namespace=sb --set-json 'agent.sandbox.api.addresses=["192.0.2.1/32"]' --set-json 'agents=[{"name":"a","routines":["x"]}]' --show-only templates/egress.yaml)
    ! "${metered[@]}" | grep -qi trial || { echo "the proxy is opened to trials nobody asked for" >&2; exit 1; }
    opened="$("${metered[@]}" --set egress.model.trials.enabled=true)"
    grep -A1 'name: METER_TRIAL_MODELS' <<<"$opened" | grep -qF "value: \"$(sed -n 's/^  model: \(claude-[a-z0-9-]*\)$/\1/p' charts/carman/values.yaml | head -1)\"" ||
        { echo "trials are not kept to the cheapest model by default" >&2; exit 1; }
    [ "$(grep -c 'name: egress-meter-reads-trials' <<<"$opened")" = 3 ] && grep -q 'name: trial-agents-reach-the-model' <<<"$opened" && awk '/name: proxy-admits-only-agents/,/^---/' <<<"$opened" | grep -q 'reaches-the-model' ||
        { echo "the meter's right over the sandbox's pods, or the trials' way to it, is not rendered" >&2; exit 1; }
    awk '/name: trial-agents-reach-the-model/,/^---/' <<<"$opened" | grep -q 'port: 3131' && ! awk '/name: trial-agents-reach-the-model/,/^---/' <<<"$opened" | grep -q 3128 ||
        { echo "trials are let out to more than the meter's port for them" >&2; exit 1; }
    if helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set egress.model.trials.enabled=true >/dev/null 2>&1; then
        echo "the chart opens the model to trials with no credential at the proxy" >&2
        exit 1
    fi
    # R5, who comes in (ADR 0066): the
    # proxy admits only the pods that leave through it, isolated or not; an
    # isolated namespace with a service behind a load balancer names who
    # that is; and the local cluster's values are isolated.
    for iso in true false; do
        helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set isolated=$iso --show-only templates/egress.yaml |
            awk '/name: proxy-admits-only-agents/,/^---/' | grep -q 'app.kubernetes.io/component: agent' || { echo "the proxy is open to more than agents (isolated: $iso)" >&2; exit 1; }
    done
    if helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set memory.service.type=LoadBalancer >/dev/null 2>&1; then
        echo "the chart renders an isolated memory service reached from outside with nobody admitted" >&2
        exit 1
    fi
    local_rules="$(helm template t charts/carman -f deploy/values/local.yaml --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 --set backup.repository=r | awk '/^kind: NetworkPolicy/{getline; getline; print $2}' | sort | tr '\n' ' ')"
    for rule in only-this-namespace-comes-in memory-admits-the-owners-machine dispatcher-admits-the-owners-machine proxy-admits-only-agents dispatcher-db-admits-only-the-dispatcher; do
        case " $local_rules " in *" $rule "*) ;; *) echo "the local cluster's values do not render the rule $rule" >&2; exit 1 ;; esac
    done
    helm template t charts/carman -f deploy/values/trial.yaml -f deploy/values/trial-agents.yaml --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 >/dev/null ||
        { echo "the trial's values for agents that reach the model do not render" >&2; exit 1; }
    helm template t charts/carman -f deploy/values/trial.yaml -f deploy/values/trial-triggers.yaml --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 |
        grep -F '\"when\":{' >/dev/null || { echo "the triggers' trial values do not render their rules" >&2; exit 1; }
    helm template t charts/carman -f deploy/values/trial.yaml --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 >/dev/null ||
        { echo "the trial's values do not render" >&2; exit 1; }
    # ADR 0061: an agent's entry may name its tools, and the chart refuses
    # what cmem routine would leave out. The trial's surveyor renders to the
    # list's own command and nothing else, and its harness is allowed that.
    named() { "${agents[@]}" --set-json "agents=[{\"name\":\"a\",\"routines\":[\"x\"],\"mcp\":{\"tr\":\"https://x\"},\"tools\":$1}]"; }
    for bad in '{"commands":["gh pr edit"]}' '{"run":["gh"]}' '{"run":["gh pr --web"]}' '{"mcp":{"other":["t"]}}' '{"read":["etc"]}' '{"bash":true}'; do
        if named "$bad" >/dev/null 2>&1; then
            echo "the chart renders an agent whose tools are $bad" >&2
            exit 1
        fi
    done
    if "${agents[@]}" --set-json 'agents=[{"name":"a","interactive":true,"tools":{"run":["gh pr view"]}}]' >/dev/null 2>&1; then
        echo "the chart renders an interactive agent with named tools" >&2
        exit 1
    fi
    surveyor="$(helm template t charts/carman -f deploy/values/trial.yaml -f deploy/values/trial-improve.yaml --set memory.image=m:0 --set agent.image=a:0 \
        --set dispatcher.image=d:0 --show-only templates/agents.yaml | awk '$1 == "name:" && $2 == "agent-watch" {w = 1} w && /name: CMEM_ROUTINE_(TOOLS|MEMORY|RUN|COMMANDS|MCP_TOOLS|READ)$/ {n = $3; getline; v = $0; sub(/^ *value: /, "", v); gsub(/"/, "", v); print n "=" v} w && /^---/ {exit}')"
    [ "$(printf %s "$surveyor" | tr '\n' ' ')" = "CMEM_ROUTINE_TOOLS=named CMEM_ROUTINE_MEMORY=off CMEM_ROUTINE_RUN=dispatcher improve" ] ||
        { echo "the trial's surveyor does not render to the list's command only: $surveyor" >&2; exit 1; }
    (unset CMEM_ROUTINE_COMMANDS CMEM_ROUTINE_MCP_TOOLS CMEM_ROUTINE_MCP_CONFIG CMEM_ROUTINE_READ
        while IFS= read -r kv; do export "$kv"; done <<<"$surveyor"
        CGO_ENABLED=0 go run ./cmd/cmem routine --flags) | grep -F "'--allowedTools' 'Bash(dispatcher improve)' 'Bash(dispatcher improve *)' '--append-system-prompt'" >/dev/null ||
        { echo "the trial's surveyor's harness is not allowed the list's command only" >&2; exit 1; }
else
    skip "the chart's checks: no helm"
fi
# The nightly backup keeps agents' sessions and kept conversations in a form
# that comes back whole (ADR 0038, RECOVERY.md). Checked where restic is.
if command -v restic >/dev/null && command -v python3 >/dev/null; then
    scripts/check-backup-copy.sh
else
    skip "the nightly backup's copy (scripts/check-backup-copy.sh): no restic or python3"
fi
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
# agent-view (ADR 0057), where there is a tmux, in servers of its own that
# read no configuration: two clients see two different windows of one pod,
# each in a view that holds that window alone, with no status line and no
# prefix, the pod's own session is left as it was, a view ends at once when
# its window closes, whichever view it is, and when its client goes, and a
# name with no window is refused. Each wait is for what the views or the
# pod's tmux say, and ends when they say it.
if command -v tmux >/dev/null; then
    view="$(mktemp -d)"
    (
        set -e
        unset TMUX TMUX_PANE
        pod() { tmux -S "$view/pod" -f /dev/null "$@"; }
        mine() { tmux -S "$view/mine" -f /dev/null "$@"; }
        gone() { pod kill-server 2>/dev/null || true; mine kill-server 2>/dev/null || true; }
        trap gone EXIT
        pod new-session -d -s main -n first -x 80 -y 24 'sleep 300'
        pod new-window -d -t main: -n second 'sleep 300'
        run="AGENT_TMUX=$(command -v tmux) AGENT_TMUX_SOCKET=$view/pod AGENT_ATTACH_DIR=$view/marks AGENT_HARNESS_DIR=$view/none sh $PWD/deploy/agent/agent-view"
        mine new-session -d -s o -n one -x 80 -y 24 "$run '' first; echo left=\$? > $view/one"
        mine new-window -d -t o: -n two "$run '' second; echo left=\$? > $view/two"
        mine new-window -d -t o: -n hold 'sleep 300'
        look() { pod list-sessions -F '#{session_name} #{session_attached} #{session_windows} #{window_name} #{status}' | sed 's/^view-[0-9]* /view /' | sort | tr '\n' ';'; }
        for _ in $(seq 300); do seen="$(look)"; [ "$seen" = "main 0 2 first on;view 1 1 first off;view 1 1 second off;" ] && break; sleep 0.1; done
        [ "$seen" = "main 0 2 first on;view 1 1 first off;view 1 1 second off;" ] || { echo "agent-view: two clients do not see two windows, each in a view of its own that holds only it: $seen" >&2; exit 1; }
        pod show-options -t main prefix | grep -q None && { echo "agent-view takes the prefix from the pod's own session" >&2; exit 1; }
        for v in $(pod list-sessions -F '#{session_name}' | grep '^view-'); do
            pod show-options -t "$v" prefix | grep -q None || { echo "a view has a prefix key" >&2; exit 1; }
        done
        # Its window closes: the view ends with it, as left, and the other
        # view and the pod's session stay as they were. Then the same the
        # other way round: which view was made last does not matter.
        pod kill-window -t main:second
        seen="$(look)"
        [ "$seen" = "main 0 1 first on;view 1 1 first off;" ] || { echo "a view does not end at once when its window closes, or more ends than it: $seen" >&2; exit 1; }
        for _ in $(seq 300); do [ -s "$view/two" ] && break; sleep 0.1; done
        [ "$(cat "$view/two" 2>/dev/null)" = left=0 ] || { echo "a view does not end, as left, when its window closes: $(cat "$view/two" 2>/dev/null)" >&2; exit 1; }
        pod new-window -d -t main: -n second 'sleep 300'
        mine new-window -d -t o: -n again "$run '' second; echo left=\$? > $view/again"
        for _ in $(seq 300); do seen="$(look)"; [ "$seen" = "main 0 2 first on;view 1 1 first off;view 1 1 second off;" ] && break; sleep 0.1; done
        pod kill-window -t main:first
        seen="$(look)"
        [ "$seen" = "main 0 1 second on;view 1 1 second off;" ] || { echo "the view made first does not end at once when its window closes: $seen" >&2; exit 1; }
        for _ in $(seq 300); do [ -s "$view/one" ] && break; sleep 0.1; done
        [ "$(cat "$view/one" 2>/dev/null)" = left=0 ] || { echo "the view made first does not end, as left, when its window closes: $(cat "$view/one" 2>/dev/null)" >&2; exit 1; }
        # Its client goes: no view is left behind.
        mine kill-window -t o:again
        for _ in $(seq 300); do [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 0 ] && break; sleep 0.1; done
        [ "$(pod list-sessions -F '#{session_name}' | tr '\n' ' ')" = "main " ] || { echo "a view is left behind when its client goes: $(pod list-sessions -F '#{session_name}' | tr '\n' ' ')" >&2; exit 1; }
        # A view whose far end promised word of itself and went silent (a
        # dropped connection) is ended; one that keeps saying so is not.
        pod new-window -d -t main: -n third 'sleep 300'
        beat="AGENT_ATTACH_ID=here/third AGENT_VIEW_BEAT=1 AGENT_VIEW_SILENCE=3 AGENT_VIEW_LOOK=1"
        mine new-window -d -t o: -n three "$beat $run '' third; echo left=\$? > $view/three"
        (for _ in 1 2 3 4 5 6; do echo; sleep 1; done; sleep 20) | env AGENT_ATTACH_ID=here/third AGENT_ATTACH_DIR="$view/marks" sh deploy/agent/agent-view --beat &
        sleep 4
        [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 1 ] || { echo "a view whose far end says it is there is ended" >&2; exit 1; }
        for _ in $(seq 100); do [ -s "$view/three" ] && break; sleep 0.1; done
        [ -s "$view/three" ] && [ "$(cat "$view/three")" != left=0 ] && [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 0 ] ||
            { echo "a view whose far end went silent is not ended, or ends as if it was left: $(cat "$view/three" 2>/dev/null)" >&2; exit 1; }
        # The far end says it has left: its view is ended at once.
        mine new-window -d -t o: -n four "$beat $run '' third; echo left=\$? > $view/four"
        for _ in $(seq 300); do [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 1 ] && break; sleep 0.1; done
        (echo; sleep 1; echo bye; sleep 5) | env AGENT_ATTACH_ID=here/third AGENT_ATTACH_DIR="$view/marks" sh deploy/agent/agent-view --beat
        for _ in $(seq 300); do [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 0 ] && break; sleep 0.1; done
        [ "$(pod list-sessions -F '#{session_name}' | grep -c '^view-')" = 0 ] || { echo "a view whose far end said it left is still there" >&2; exit 1; }
        code=0; eval "$run '' nosuch" >/dev/null 2>&1 || code=$?
        [ "$code" = 3 ] || { echo "agent-view of a name with no window ends with $code, not 3" >&2; exit 1; }
    ) || { rm -rf "$view"; exit 1; }
    rm -rf "$view"
else
    skip "agent-view: no tmux"
fi
# agent-wake (ADR 0075), where there
# is a tmux, in servers of its own: a line is typed into the window of the
# process named, found from a child of what the window runs, where the
# window is at an empty prompt and nobody looks at it; a line a person left
# half typed is not sent off, a window a client looks at is left alone, and
# a process in no window is said to be in none.
if command -v tmux >/dev/null; then
    wake="$(mktemp -d)"
    (
        set -e
        unset TMUX TMUX_PANE
        pod() { tmux -S "$wake/pod" -f /dev/null "$@"; }
        mine() { tmux -S "$wake/mine" -f /dev/null "$@"; }
        gone() { pod kill-server 2>/dev/null || true; mine kill-server 2>/dev/null || true; }
        trap gone EXIT
        # What stands for the harness: its prompt, a child of its own, and
        # the line it is given written down.
        cat > "$wake/harness" <<'SH'
#!/bin/sh
sleep 300 &
echo $! > "$1.pid"
printf '\342\235\257\302\240'
IFS= read -r line
printf '%s\n' "$line" > "$1"
sleep 300
SH
        chmod +x "$wake/harness"
        pod new-session -d -s main -n free -x 100 -y 24 "$wake/harness $wake/free"
        pod new-window -d -t main: -n half "$wake/harness $wake/half"
        pod new-window -d -t main: -n seen "$wake/harness $wake/seen"
        pod select-window -t main:=seen
        mine new-session -d -s o -x 100 -y 24 "tmux -S $wake/pod -f /dev/null attach -t main"
        for _ in $(seq 100); do [ -s "$wake/free.pid" ] && [ -s "$wake/half.pid" ] && [ -s "$wake/seen.pid" ] && [ -n "$(pod list-clients)" ] && break; sleep 0.1; done
        [ -n "$(pod list-clients)" ] || { echo "agent-wake: the check's client did not attach" >&2; exit 1; }
        sleep 0.3
        run() { AGENT_TMUX="$(command -v tmux)" AGENT_TMUX_SOCKET="$wake/pod" sh deploy/agent/agent-wake "$@" >/dev/null 2>&1; echo $?; }
        pod send-keys -t main:=half -l 'a person is half way'
        sleep 0.3
        code="$(run "$(cat "$wake/half.pid")" 'the line')"
        [ "$code" = 5 ] && [ ! -e "$wake/half" ] || { echo "agent-wake typed over a line left half typed, or ended with $code, not 5" >&2; exit 1; }
        code="$(AGENT_WAKE_TELL=1 run "$(cat "$wake/seen.pid")" 'the line')"
        [ "$code" = 4 ] && [ ! -e "$wake/seen" ] || { echo "agent-wake typed into a window a client looks at, or ended with $code, not 4" >&2; exit 1; }
        code="$(run $$ 'the line')"
        [ "$code" = 3 ] || { echo "agent-wake of a process in no window ends with $code, not 3" >&2; exit 1; }
        code="$(run "$(cat "$wake/free.pid")" 'the line the watcher types')"
        for _ in $(seq 50); do [ -s "$wake/free" ] && break; sleep 0.1; done
        [ "$code" = 0 ] && [ "$(cat "$wake/free" 2>/dev/null)" = 'the line the watcher types' ] ||
            { echo "agent-wake did not type its line into a free window at its prompt: exit $code, got '$(cat "$wake/free" 2>/dev/null)'" >&2; exit 1; }
        # Once the person has left, their window is typed into too.
        mine kill-server
        for _ in $(seq 50); do [ -z "$(pod list-clients)" ] && break; sleep 0.1; done
        code="$(run "$(cat "$wake/seen.pid")" 'after they left')"
        for _ in $(seq 50); do [ -s "$wake/seen" ] && break; sleep 0.1; done
        [ "$code" = 0 ] && [ "$(cat "$wake/seen" 2>/dev/null)" = 'after they left' ] || { echo "agent-wake does not type once the client has gone: exit $code" >&2; exit 1; }
    ) || { rm -rf "$wake"; exit 1; }
    rm -rf "$wake"
else
    skip "agent-wake: no tmux"
fi
# The task stream's runner waits for the plan's limit to reset where the
# dispatcher says a task's run stopped at it (ADR 0069), and not otherwise.
if command -v python3 >/dev/null; then
    limited='  12  2026-11-28T01:00:05Z  message.limited    {"message":"m","words":"You have hit your limit","resets":"2026-11-28T03:00:00Z"}'
    [ "$(echo "$limited" | python3 bench/stream/limit.py 2026-11-28T01:00:00Z)" = "wait 7320" ] &&
        [ "$(echo '  3  t  message.limited  {"message":"m","resets":"0001-01-01T00:00:00Z"}' | python3 bench/stream/limit.py)" = wait-unknown ] &&
        [ "$(echo '  3  t  message.delivered  {}' | python3 bench/stream/limit.py)" = none ] ||
        { echo "bench/stream/limit.py does not tell a run stopped at the plan's limit from one that was not" >&2; exit 1; }
fi
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
else
    skip "agent-open --reopen over a window that runs something: no /proc"
fi
kill "$idle" "$job" "$harness" 2>/dev/null || true
# agent-open --model: remembered with the session, kept when it is opened
# again without one, given to the harness when its window runs, and cleared.
printf '#!/bin/sh\necho "$*" >> "$FAKE/harness"\n' > "$fake/claude"
mkdir -p "$fake/home"
open() { FAKE="$fake" AGENT_TMUX="$fake/tmux" AGENT_SESSIONS_FILE="$fake/list" HOME="$fake/home" sh deploy/agent/agent-open "$@" >/dev/null 2>&1; }
run() { (cd "$fake" && FAKE="$fake" PATH="$fake:$PATH" SHELL=/bin/true AGENT_ENV_FILE=/dev/null AGENT_TMUX="$fake/tmux" AGENT_SESSIONS_FILE="$fake/list" HOME="$fake/home" \
    sh "$OLDPWD/deploy/agent/agent-open" --run trial >/dev/null 2>&1); }
open --model cheap-model trial "$fake" && open trial "$fake" && [ "$(awk -F '\t' '$1 == "trial" {print $3}' "$fake/list")" = cheap-model ] ||
    { rm -rf "$fake"; echo "agent-open does not remember a session's model, or loses it when the session is opened again" >&2; exit 1; }
run; grep -qx -- '--name trial --model cheap-model' "$fake/harness" || { rm -rf "$fake"; echo "agent-open does not give the harness the session's model: $(cat "$fake/harness" 2>/dev/null)" >&2; exit 1; }
open --model default trial "$fake" && : > "$fake/harness" && run && grep -qx -- '--name trial' "$fake/harness" ||
    { rm -rf "$fake"; echo "agent-open --model default does not give the session back the pod's model" >&2; exit 1; }
rm -rf "$fake"
# ADR numbers (scripts/adr-number.sh): no two records share one; one not
# numbered yet is named, and fails --strict, for the merge to number it.
dup="$(ls docs/adr | sed -n 's/^\([0-9]\{4\}\)-.*/\1/p' | uniq -d)"
[ -z "$dup" ] || { echo "two ADRs are numbered $dup: scripts/adr-number.sh OLD NEW renumbers one" >&2; exit 1; }
for f in docs/adr/new-*.md; do [ ! -e "$f" ] || skip "the number of $f: scripts/adr-number.sh at the merge"; done
# The script, in a repository of its own: a new record numbered with its
# references, and a branch's clashing number moved without touching main's.
adr="$(mktemp -d)"
(
    set -e
    export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
    script="$PWD/scripts/adr-number.sh"
    cd "$adr" && git init -q -b main && mkdir docs docs/adr
    echo "# One" > docs/adr/0001-one.md && printf '| [0001](0001-one.md) | One |\n' > docs/adr/README.md && echo "As ADR 0001 says." > notes.md
    git add -A && git commit -q -m base && base="$(git rev-parse HEAD)"
    echo "# Two, after ADR 0001" > docs/adr/0002-two.md
    echo "# Mine" > docs/adr/new-mine.md
    printf '| [0002](0002-two.md) | Two |\n| [new-mine](new-mine.md) | Mine |\n' >> docs/adr/README.md
    echo "ADR new-mine and ADR 0002 (ADRs 0001, 0002)." >> notes.md
    git add -A && git commit -q -m branch
    # In a list after ||, set -e does not hold: each step is chained.
    bash "$script" >/dev/null && bash "$script" 0002 0004 "$base" >/dev/null &&
        [ -f docs/adr/0003-mine.md ] && [ -f docs/adr/0004-two.md ] && [ ! -e docs/adr/new-mine.md ] &&
        [ "$(cat notes.md)" = "$(printf 'As ADR 0001 says.\nADR 0003 and ADR 0004 (ADRs 0001, 0004).')" ] &&
        grep -qxF '| [0003](0003-mine.md) | Mine |' docs/adr/README.md && grep -qxF '| [0004](0004-two.md) | Two |' docs/adr/README.md &&
        grep -qxF '| [0001](0001-one.md) | One |' docs/adr/README.md && grep -qxF '# Two, after ADR 0001' docs/adr/0004-two.md &&
        ! bash "$script" 0001 0009 "$base" 2>/dev/null
) || { rm -rf "$adr"; echo "scripts/adr-number.sh does not number a new ADR, or renumber a branch's, as it should" >&2; exit 1; }
rm -rf "$adr"
# child-harness: the model and the prompt, no hooks, none of the parent's
# session, the proxy's placeholder as the credential.
child="$(mktemp -d)"
printf '#!/bin/sh\necho "$*|$CLAUDE_CODE_OAUTH_TOKEN|$CMEM_HOOK|${CLAUDECODE:-}${DISPATCHER_SESSION_NAME:-}"\n' > "$child/claude" && chmod +x "$child/claude"
seen="$(PATH="$child:$PATH" AGENT_MODEL_PLACEHOLDER=placeholder CLAUDECODE=1 DISPATCHER_SESSION_NAME=parent sh deploy/agent/child-harness cheap 'say ok' --max-turns 1)"
rm -rf "$child"
[ "$seen" = "-p say ok --model cheap --max-turns 1|placeholder|off|" ] || { echo "child-harness does not start the child as it should: $seen" >&2; exit 1; }
go vet ./...
[ -n "${DISPATCH_TEST_DATABASE_URL:-}" ] ||
    skip "the dispatcher's Postgres suite (internal/dispatch/pgxdispatch): DISPATCH_TEST_DATABASE_URL is not set"
status=0
go test ./... "$@" || status=$?
if [ ${#skipped[@]} -gt 0 ]; then
    {
        echo
        echo "=== NOT EVERYTHING WAS CHECKED: ${#skipped[@]} check(s) skipped ==="
        printf '  - %s\n' "${skipped[@]}"
        [ -n "$strict" ] && echo "=== --strict: a skip is a failure ===" || echo "=== scripts/test.sh --strict fails on a skip ==="
    } >&2
    [ -z "$strict" ] || [ "$status" -ne 0 ] || status=1
fi
exit "$status"
