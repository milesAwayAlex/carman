#!/usr/bin/env bash
# Runs the memory service on this machine, for trying it out. The library (a
# git repository of markdown notes) lives in .local/library unless
# CMEM_LIBRARY says otherwise, so what you store survives restarts. Extra
# arguments go to `cmem serve`, for example --listen 127.0.0.1:9000.
#
# It listens on port 8378. Port 8377 is the real service (deploy/README.md),
# whose library should never receive a test note.
#
# In another shell:
#   export CMEM_URL=http://127.0.0.1:8378 CMEM_TOKEN=dev-laptop
#   bin/cmem recall anything
set -euo pipefail

cd "$(dirname "$0")/.."

# Development tokens. Set CMEM_TOKENS yourself for anything that is not local.
export CMEM_TOKENS="${CMEM_TOKENS:-laptop=dev-laptop,worker=dev-worker,dreamer=dev-dreamer}"
export CMEM_LIBRARY="${CMEM_LIBRARY:-.local/library}"

go build -o bin/cmem ./cmd/cmem
exec bin/cmem serve --listen 127.0.0.1:8378 "$@"
