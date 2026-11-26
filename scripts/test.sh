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
# The chart hands the dispatcher who may write to each agent (ADR 0047), and
# refuses a list that names an agent there is not. Checked where helm is.
if command -v helm >/dev/null; then
    render=(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0
        --set dispatcher.image=d:0 --set backup.repository=r --show-only templates/dispatcher.yaml)
    "${render[@]}" --set-json 'agents=[{"name":"a","routines":["x"]},{"name":"b","routines":["x"],"writtenBy":["a"]}]' |
        grep -qF 'value: "{\"b\":[\"a\"]}"' || { echo "the chart does not hand the dispatcher agent b's writtenBy" >&2; exit 1; }
    if "${render[@]}" --set-json 'agents=[{"name":"b","routines":["x"],"writtenBy":["nobody"]}]' >/dev/null 2>&1; then
        echo "the chart renders a writtenBy that names no agent" >&2
        exit 1
    fi
    # The owner's machine (ADR 0049): its writtenBy, a name of its own, and
    # a way in from outside only where the addresses are given.
    "${render[@]}" --set-json 'agents=[{"name":"a","routines":["x"]}]' --set-json 'dispatcher.host.writtenBy=["a"]' |
        grep -qF 'value: "{\"host\":[\"a\"]}"' || { echo "the chart does not hand the dispatcher the host's writtenBy" >&2; exit 1; }
    if "${render[@]}" --set-json 'agents=[{"name":"host","routines":["x"]}]' >/dev/null 2>&1; then
        echo "the chart renders a place for the owner's machine named for an agent" >&2
        exit 1
    fi
    if "${render[@]}" --set dispatcher.service.type=LoadBalancer >/dev/null 2>&1; then
        echo "the chart renders an isolated dispatcher reached from outside with no addresses to admit" >&2
        exit 1
    fi
    "${render[@]}" --set dispatcher.service.type=LoadBalancer --set-json 'dispatcher.service.admitFrom=["10.0.0.0/8"]' |
        grep -qF 'cidr: "10.0.0.0/8"' || { echo "the chart does not admit the owner's machine to the dispatcher" >&2; exit 1; }
else
    echo "helm is not here: the chart's checks are skipped" >&2
fi
# Nothing here needs a C compiler, and not every place that runs this has one.
export CGO_ENABLED="${CGO_ENABLED:-0}"
go vet ./...
go test ./... "$@"
