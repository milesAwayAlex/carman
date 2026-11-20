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
go vet ./...
go test ./... "$@"
