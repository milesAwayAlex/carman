#!/usr/bin/env bash
# What a run says it used against the proxy's record of its pod (ADR 0050),
# end to end with a real harness and no model: a trial with a proxy and
# meter of its own in front of the stand-in for the model's API (ADR 0068),
# whose answers a message's own words script. An agent of the trial is sent
# four messages, each a run of a kind:
#
#   plain          one call
#   a subagent     the harness starts one, whose calls its result's own
#                  usage leaves out
#   left running   a command left running ends after the result: the
#                  harness goes on, and prints a second result for that part
#   cut short      the harness is killed before any result
#
# The first three have to agree with the proxy's record to the token; the
# fourth has to be said to differ as a run cut short, and not to DISAGREE.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-run-report.sh CONTEXT    stands the trial up, checks, and leaves it up
#   scripts/trial.sh CONTEXT down                                 afterwards
#
# TRIAL_AGENT_BASE is an image with the harness, the installation's warm
# image for one. Nothing is asked of a model and nothing is spent.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-run-report: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-run-report.sh CONTEXT"
[ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_AGENT_BASE, an image with the harness"
step() { echo "ok: $*"; }

# One agent that a message starts, and tallies that close soon, so that a
# pod's record is in seconds after its last call.
values="$(mktemp)"
cat > "$values" <<'YAML'
agents:
  - name: worker
    takesMessages: true
    tools: all
    models: [claude-haiku-5-5]
    budgetUSD: 0.3
    deadlineSeconds: 300
dispatcher:
  maxRunning: 4
egress:
  model:
    idle: 5s
    longest: 20s
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
send() { owner send worker "$1" | sed -n 's/^\([0-9a-f-]*\) delivered.*/\1/p'; }
plain="$(send 'Say ok.')"
subagent="$(send 'Do it.
stand-in: use Agent {"description":"a part","prompt":"say ok","subagent_type":"general-purpose"}')"
left="$(send 'Do it.
stand-in: use Bash {"command":"sleep 8; echo done","run_in_background":true}')"
cut="$(send 'Do it.
stand-in: use Bash {"command":"echo one"}
stand-in: use Bash {"command":"kill -9 $PPID"}')"
[ -n "$plain" ] && [ -n "$subagent" ] && [ -n "$left" ] && [ -n "$cut" ] || die "a message was not delivered"

# Each run's own report: how it counted.
report() {  # MESSAGE
    local out
    for _ in $(seq 90); do
        out="$(owner log --type run.usage --last 50 2>/dev/null | grep -F "\"message\":\"$1\"" | tail -1 || true)"
        [ -n "$out" ] && { echo "$out"; return 0; }
        sleep 2
    done
    die "the run for message $1 recorded no usage; the log has:
$(owner log --last 30)"
}
for m in "$plain" "$subagent" "$left"; do
    grep -qF '"basis":"whole"' <<<"$(report "$m")" || die "the run for $m did not count the whole of what its process used: $(report "$m")"
done
grep -qF '"basis":"stream"' <<<"$(report "$cut")" || die "the run cut short did not count what its stream showed: $(report "$cut")"
step "four runs recorded what they used: three counted the whole of their process's calls, the one cut short what its stream had shown"

# Against the proxy's record, once it is in for all four. (A trial that
# has run before has other runs in its log: take it down first.)
for _ in $(seq 60); do
    status="$(owner status)"
    said="$(sed -n '/runs against the proxy/,/^running:/p' <<<"$status")"
    grep -q "3 agree, 1 differ" <<<"$said" && break
    sleep 3
done
of() { grep -A2 -F "message-$1" <<<"$said" || true; }   # what status says of a message's run, if it differs
for m in "$plain" "$subagent" "$left"; do
    [ -z "$(of "$m")" ] || die "a run that counted the whole differs from the proxy's record:
$(of "$m")"
done
grep -q "runs against the proxy's record, last seven days: 3 agree, 1 differ in a way that is known$" <<<"$said" || die "status does not say three agree and one differs in a known way:
$said"
grep -qF "the run was cut short: it counted what its stream had shown" <<<"$(of "$cut")" || die "the run cut short is not said to be one:
$said"
step "the plain run, the one with a subagent and the one that went on after its result agree with the proxy's record to the token"
step "the run cut short is said to differ as one: $(of "$cut" | tr -s ' ' | tr '\n' ';')"
echo "a run's count is the whole of what its process used, in the trial $(scripts/trial.sh "$context" name). Take it down with: scripts/trial.sh $context down"
