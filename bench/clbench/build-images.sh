#!/usr/bin/env bash
# Builds the two benchmark images if they are missing (or all of them with
# --force). Build contexts go under CARMAN_BENCH_ROOT, which has to be a
# directory the container runtime can see; when `docker` runs inside a virtual
# machine that is often one shared directory and nothing else.
set -euo pipefail
cd "$(dirname "$0")/../.."

if [ -f .local/bench.env ]; then
    set -a
    # shellcheck disable=SC1091
    . .local/bench.env
    set +a
fi

VERSION=2.1.311
ROOT="${CARMAN_BENCH_ROOT:-$HOME/carman-bench}"
CLAUDE_IMG="carman-clbench-claude:$VERSION"
CMEM_IMG="carman-clbench-cmem:$VERSION"
force=0
[ "${1:-}" = "--force" ] && force=1

have() { docker image inspect "$1" >/dev/null 2>&1; }

mkdir -p "$ROOT/build/claude" "$ROOT/build/cmem"

if [ $force = 1 ] || ! have "$CLAUDE_IMG"; then
  cp bench/clbench/docker/Dockerfile.claude "$ROOT/build/claude/Dockerfile"
  docker build -t "$CLAUDE_IMG" --build-arg "CLAUDE_CODE_VERSION=$VERSION" "$ROOT/build/claude"
fi

# Always rebuilt, so the container never runs a stale cmem. It is one small
# layer on top of the Claude Code image and takes a few seconds.
cp bench/clbench/docker/Dockerfile.cmem "$ROOT/build/cmem/Dockerfile"
if command -v go >/dev/null; then
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$ROOT/build/cmem/cmem" ./cmd/cmem
else
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 nix develop -c go build -o "$ROOT/build/cmem/cmem" ./cmd/cmem
fi
docker build -q -t "$CMEM_IMG" --build-arg "BASE=$CLAUDE_IMG" "$ROOT/build/cmem" >/dev/null
echo "images ready: $CLAUDE_IMG $CMEM_IMG"
