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
go vet ./...
go test ./... "$@"
