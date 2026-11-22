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
named="$(grep -rli carman charts/carman/templates charts/carman/values.yaml deploy/agent deploy/Dockerfile || true)"
if [ -n "$named" ]; then
    echo "These name the project, and what runs in a cluster must not:" >&2
    echo "$named" >&2
    exit 1
fi
# The agent image is built from a list of files in two places, the deploy
# script and the images workflow: what the Dockerfile copies must be in both.
for file in $(sed -n 's/^COPY \(.*\) [^ ]*$/\1/p' deploy/agent/Dockerfile | tr ' ' '\n' | grep -v -e '^cmem-' -e '^routines$'); do
    for list in scripts/deploy.sh .github/workflows/images.yml; do
        grep -q -E "deploy/agent/$file( |\$)" "$list" || { echo "$list does not copy deploy/agent/$file, which the agent image needs" >&2; exit 1; }
    done
done
# Nothing here needs a C compiler, and not every place that runs this has one.
export CGO_ENABLED="${CGO_ENABLED:-0}"
go vet ./...
go test ./... "$@"
