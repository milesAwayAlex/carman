#!/usr/bin/env bash
# Installs the carman arms into the Continual Learning Bench clone by symlink.
# The registry finds a system by the name of a file or package in src/systems,
# so there is one symlinked package (_carman, skipped by discovery) and one
# symlinked file per arm. Nothing else in the clone is changed.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
clone="${CLBENCH_DIR:-$repo/.local/clbench}"
dest="$clone/src/systems"
[ -d "$dest" ] || { echo "no Continual Learning Bench clone at $clone" >&2; exit 1; }

ln -sfn "$here/systems/_carman" "$dest/_carman"
for arm in none builtin cmem cmem_hook; do
  ln -sf "$here/systems/carman_$arm.py" "$dest/carman_$arm.py"
done
echo "installed carman arms into $dest"

# Our own schedules, named <task>.<schedule>.json here, linked into the task.
for schedule in "$here"/schedules/*.json; do
  [ -e "$schedule" ] || continue
  name="$(basename "$schedule")"
  task="${name%%.*}"
  ln -sf "$schedule" "$clone/src/tasks/$task/schedules/${name#*.}"
done
