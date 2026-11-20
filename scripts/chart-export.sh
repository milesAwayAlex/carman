#!/usr/bin/env bash
# Writes the chart out under another name, for an installation that keeps the
# charts it deploys in a repository of its own and calls this one what it
# likes there.
#
#   scripts/chart-export.sh NAME DIR [DESCRIPTION]     makes DIR/NAME
#
# Only Chart.yaml says what the chart is called; the templates and values
# never name this project (scripts/test.sh checks), so the copy says NAME
# and nothing else. The copy is whole: it needs nothing from this repository.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "chart-export: $*" >&2; exit 1; }

name="${1:-}" dir="${2:-}"
description="${3:-A memory service for agents, and the agents that run beside it}"
[ -n "$name" ] && [ -n "$dir" ] || die "say: scripts/chart-export.sh NAME DIR"
out="$dir/$name"
[ ! -e "$out" ] || die "$out already exists; a copy is never written over another"

mkdir -p "$out"
cp -R charts/carman/templates charts/carman/values.yaml "$out/"
version="$(sed -n 's/^version: //p' charts/carman/Chart.yaml)"
cat > "$out/Chart.yaml" <<CHART
apiVersion: v2
name: $name
description: $description
type: application
version: $version
appVersion: "$version"
CHART
if grep -rli carman "$out" >/dev/null; then
    rm -rf "$out"
    die "the chart names this project outside Chart.yaml; that is a fault in the chart"
fi
echo "wrote $out: $(find "$out" -type f | wc -l | tr -d ' ') files, from commit $(git rev-parse --short HEAD)$(git diff --quiet -- charts || echo ' with uncommitted changes')"
