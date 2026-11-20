#!/usr/bin/env bash
# Runs the transfer benchmark (bench/transfer): is a lesson learned in one
# session used in another? Arguments go to bench/transfer/run.py, for example:
#
#   scripts/bench-transfer.sh run --arms none,oracle --cases kubectl-context
#   scripts/bench-transfer.sh consolidate
#   scripts/bench-transfer.sh distill --dreamer-models claude-haiku-5-5,claude-sonnet-5-5
#   scripts/bench-transfer.sh show PATH/probe-same.jsonl
#
# `run` and `consolidate` call a model on the owner's Claude subscription. The token comes from
# CLAUDE_CODE_OAUTH_TOKEN, or the file .local/bench-token (mode 600), or a
# command that prints it (CARMAN_BENCH_TOKEN_CMD, for a keychain or another
# secret store that needs nobody at the keyboard), or 1Password via
# CARMAN_BENCH_TOKEN_REF, in that order; the last two are set in
# .local/bench.env. It is never printed or written to disk.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "bench-transfer: $*" >&2; exit 1; }

# Local, git-ignored settings: where benchmark and build files go on this
# machine (CARMAN_BENCH_ROOT), and where the token comes from. Exported so the
# programs started below see them.
if [ -f .local/bench.env ]; then
    set -a
    # shellcheck disable=SC1091
    . .local/bench.env
    set +a
fi

if [ "${1:-}" = run ] || [ "${1:-}" = consolidate ] || [ "${1:-}" = distill ]; then
    [ -z "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is set; the benchmark must run on the subscription token only. Unset it."
    if pgrep -f "bench/transfer/run.py (run|consolidate|distill)|clbench (run|smoke)" >/dev/null; then
        die "another benchmark run is in progress; wait for it to finish"
    fi
    # The host's cmem and the image's cmem are rebuilt so neither is stale.
    if command -v go >/dev/null; then
        go build -o bin/cmem ./cmd/cmem
    else
        nix develop -c go build -o bin/cmem ./cmd/cmem
    fi
    bench/clbench/build-images.sh >/dev/null
    if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
        file="${CARMAN_BENCH_TOKEN_FILE:-.local/bench-token}"
        if [ -f "$file" ]; then
            [ "$(stat -f %Lp "$file" 2>/dev/null || stat -c %a "$file")" = 600 ] || die "$file must have mode 600"
            CLAUDE_CODE_OAUTH_TOKEN="$(cat "$file")"
        elif [ -n "${CARMAN_BENCH_TOKEN_CMD:-}" ]; then
            CLAUDE_CODE_OAUTH_TOKEN="$(sh -c "$CARMAN_BENCH_TOKEN_CMD")" && [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] ||
                die "the command in CARMAN_BENCH_TOKEN_CMD did not print a token"
        elif [ -n "${CARMAN_BENCH_TOKEN_REF:-}" ]; then
            CLAUDE_CODE_OAUTH_TOKEN="$(op read "$CARMAN_BENCH_TOKEN_REF")" || die "could not read the token from 1Password"
        else
            die "no token: export CLAUDE_CODE_OAUTH_TOKEN, or put it in .local/bench-token (mode 600), or set CARMAN_BENCH_TOKEN_CMD or CARMAN_BENCH_TOKEN_REF in .local/bench.env"
        fi
        export CLAUDE_CODE_OAUTH_TOKEN
    fi
fi

# Several commands can share one token read, separated by --then:
#   scripts/bench-transfer.sh run --arms cmem --then consolidate
segment=()
for arg in "$@" --then; do
    if [ "$arg" = --then ]; then
        [ ${#segment[@]} -eq 0 ] || /usr/bin/env python3 bench/transfer/run.py "${segment[@]}"
        segment=()
    else
        segment+=("$arg")
    fi
done
