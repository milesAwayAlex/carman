#!/usr/bin/env bash
# Runs the task-stream benchmark (Continual Learning Bench) for one arm of the
# memory comparison. See bench/clbench/README.md.
#
#   scripts/bench-stream.sh smoke   TASK ARM [clbench options]   one or two turns, a wiring check
#   scripts/bench-stream.sh run     TASK ARM [clbench options]   a real run
#   scripts/bench-stream.sh dry-run TASK ARM [clbench options]   validate without calling a model
#
# ARM is one of: none, builtin, cmem, cmem_hook; or several separated by
# commas; or all. With several, the token is fetched once.
#
# smoke and run call a model on the owner's Claude subscription. The token is
# taken from CLAUDE_CODE_OAUTH_TOKEN, or the file .local/bench-token (mode 600),
# or a command that prints it (CARMAN_BENCH_TOKEN_CMD), or 1Password via the
# reference in CARMAN_BENCH_TOKEN_REF, in that order. It is never printed or
# written to disk, and reaches containers by name only.
set -euo pipefail

cd "$(dirname "$0")/.."
repo="$PWD"
clone="${CLBENCH_DIR:-$repo/.local/clbench}"
die() { echo "bench-stream: $*" >&2; exit 1; }

[ $# -ge 3 ] || die "usage: scripts/bench-stream.sh smoke|run|dry-run TASK ARM [clbench options]"
mode="$1" task="$2" arms="$3"
shift 3
[ "$arms" = all ] && arms="none,builtin,cmem,cmem_hook"
arms="${arms//,/ }"
for arm in $arms; do
    case "$arm" in none | builtin | cmem | cmem_hook) ;; *) die "unknown arm '$arm' (none, builtin, cmem, cmem_hook, or all)" ;; esac
done
case "$mode" in smoke | run | dry-run) ;; *) die "unknown mode '$mode' (smoke, run, dry-run)" ;; esac
[ -x "$clone/.venv/bin/clbench" ] || die "no Continual Learning Bench environment at $clone; see bench/clbench/README.md"

# Local, git-ignored settings: where benchmark and build files go on this
# machine (CARMAN_BENCH_ROOT), and where the token comes from. Exported so the
# programs started below see them.
if [ -f .local/bench.env ]; then
    set -a
    # shellcheck disable=SC1091
    . .local/bench.env
    set +a
fi

# Set up: the arms installed in the clone, the images, and the host's cmem.
bench/clbench/install.sh >/dev/null
bench/clbench/build-images.sh >/dev/null
if command -v go >/dev/null; then
    go build -o bin/cmem ./cmd/cmem
else
    nix develop -c go build -o bin/cmem ./cmd/cmem
fi

[ -z "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is set; the benchmark must run on the subscription token only. Unset it."
if [ "$mode" = dry-run ]; then
    # Nothing is sent anywhere in a dry run, but the arms insist on a token.
    export CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-dry-run-placeholder}"
elif [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ ! -f "${CARMAN_BENCH_TOKEN_FILE:-$repo/.local/bench-token}" ]; then
    if [ -n "${CARMAN_BENCH_TOKEN_CMD:-}" ]; then
        CLAUDE_CODE_OAUTH_TOKEN="$(sh -c "$CARMAN_BENCH_TOKEN_CMD")" && [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] ||
            die "the command in CARMAN_BENCH_TOKEN_CMD did not print a token"
    else
        [ -n "${CARMAN_BENCH_TOKEN_REF:-}" ] || die "no token: export CLAUDE_CODE_OAUTH_TOKEN, or put it in .local/bench-token (mode 600), or set CARMAN_BENCH_TOKEN_CMD or CARMAN_BENCH_TOKEN_REF in .local/bench.env"
        CLAUDE_CODE_OAUTH_TOKEN="$(op read "$CARMAN_BENCH_TOKEN_REF")" || die "could not read the token from 1Password"
    fi
    export CLAUDE_CODE_OAUTH_TOKEN
fi

# One benchmark at a time. Two at once share the container tooling's single
# connection, and the first attempt at that lost whole runs.
if [ "$mode" != dry-run ] && pgrep -f "clbench (run|smoke)" >/dev/null; then
    die "another benchmark run is in progress; wait for it to finish"
fi

export CARMAN_REPO="$repo"
cd "$clone"
failed=""
failed_runs() { find "results/$task" -name 'failed_*.json' 2>/dev/null | wc -l | tr -d ' '; }
for arm in $arms; do
    case "$mode" in
        smoke) cmd=(smoke "$task" --system "carman_$arm" "$@") ;;
        run) cmd=(run "$task" --system "carman_$arm" --max-workers 1 "$@") ;;
        dry-run) cmd=(run "$task" --system "carman_$arm" --max-workers 1 --dry-run "$@") ;;
    esac
    echo "bench-stream: clbench ${cmd[*]}" >&2
    before="$(failed_runs)"
    # One arm failing should not stop the others; the summary names it. The
    # benchmark can exit cleanly after a run has failed, so count its failure
    # records as well as its exit status.
    .venv/bin/clbench "${cmd[@]}" || failed="$failed $arm"
    [ "$(failed_runs)" = "$before" ] || failed="$failed $arm(failed-run)"
done
echo "bench-stream: results under $clone/results/$task; token usage and libraries under ${CARMAN_BENCH_ROOT:-$HOME/carman-bench}" >&2
[ -z "$failed" ] || die "these arms failed:$failed"
