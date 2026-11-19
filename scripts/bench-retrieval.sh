#!/usr/bin/env bash
# Scores search and flashback against the retrieval set in bench/retrieval.
# No inference is involved, so this is free to run as often as you like.
set -euo pipefail

cd "$(dirname "$0")/.."

go test ./internal/memory -run '^TestRetrievalSet$' -count=1 -v | grep -v -e '^=== ' -e '^--- ' -e '^PASS$' -e 'retrieval_test.go'
