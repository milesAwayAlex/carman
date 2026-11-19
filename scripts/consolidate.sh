#!/usr/bin/env bash
# Runs one consolidation pass over the memory (routines/consolidate.md) in a
# Claude Code session that holds the consolidation credential.
#
#   CMEM_URL=http://127.0.0.1:8377 CMEM_DREAMER_TOKEN=... scripts/consolidate.sh [--model MODEL]
#
# The session can run `cmem` and nothing else. Afterwards:
#   cmem consolidate log            what the run changed
#   cmem consolidate revert RUN WHY undo all of it (needs the same credential)
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "consolidate: $*" >&2; exit 1; }

model=sonnet
[ "${1:-}" = "--model" ] && model="${2:?--model needs a value}"
[ -n "${CMEM_URL:-}" ] || die "set CMEM_URL to the memory service's address"
[ -n "${CMEM_DREAMER_TOKEN:-}" ] || die "set CMEM_DREAMER_TOKEN to the consolidation role's token"
command -v cmem >/dev/null || die "cmem is not on PATH"

# The hook and built-in memory are for working sessions; this one only tidies.
CMEM_TOKEN="$CMEM_DREAMER_TOKEN" CMEM_HOOK=off CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 \
    claude -p "$(cat routines/consolidate.md)" --model "$model" --allowedTools "Bash(cmem *)"
