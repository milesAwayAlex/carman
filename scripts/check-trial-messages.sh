#!/usr/bin/env bash
# Agents of a trial write to each other (ADR 0045, 0047), end to end with a
# real harness and no model: a trial with a proxy of its own in front of
# the stand-in (ADR 0068), whose answers a message's own words script. The
# agents are entries of the trial's values, their pods the chart's, each
# with the token the cluster gives it; nothing is made by hand.
#
#   asker      writes to answerer when told to
#   answerer   may be written to by asker (writtenBy), and answers
#   stranger   is in nobody's writtenBy
#
# Tried: the owner's message starts a run of asker; asker's own command
# reaches answerer, as asker; answerer's run answers; the answer comes back
# to asker's conversation, which is started again for it (in a new one,
# told so, where conversations are not kept: a trial keeps none); an
# exchange of answers ends by itself; and stranger's message to answerer is
# refused and says why.
#
#   TRIAL_AGENT_BASE=IMAGE scripts/check-trial-messages.sh CONTEXT    stands the trial up, checks, and leaves it up
#   scripts/trial.sh CONTEXT down                                     afterwards
#
# TRIAL_AGENT_BASE is an image with the harness. Nothing is asked of a model.
set -euo pipefail

cd "$(dirname "$0")/.."
die() { echo "check-trial-messages: $*" >&2; exit 1; }
context="${1:-}"
[ -n "$context" ] || die "say: TRIAL_AGENT_BASE=IMAGE scripts/check-trial-messages.sh CONTEXT"
[ -n "${TRIAL_AGENT_BASE:-}" ] || die "set TRIAL_AGENT_BASE, an image with the harness"
step() { echo "ok: $*"; }

values="$(mktemp)"
cat > "$values" <<'YAML'
agents:
  - name: asker
    takesMessages: true
    tools: all
    models: [claude-haiku-5-5]
    deadlineSeconds: 300
  - name: answerer
    takesMessages: true
    tools: all
    writtenBy: [asker]
    models: [claude-haiku-5-5]
    deadlineSeconds: 300
  - name: stranger
    takesMessages: true
    tools: all
    models: [claude-haiku-5-5]
    deadlineSeconds: 300
dispatcher:
  maxRunning: 3
YAML
TRIAL_PROXY=1 TRIAL_MODEL="" TRIAL_VALUES="$values" scripts/trial.sh "$context" up | tail -1
rm -f "$values"

owner() { scripts/trial.sh "$context" deploy dispatch "$@"; }
events() { owner log --type "$1" --last 100; }
await() {  # TYPE WORDS... : the latest event of the type with every one of the words
    local type="$1" out; shift
    for _ in $(seq 60); do
        out="$(events "$type" 2>/dev/null || true)"
        for w in "$@"; do out="$(grep -F -- "$w" <<<"$out" || true)"; done
        [ -z "$out" ] || { tail -1 <<<"$out"; return 0; }
        sleep 2
    done
    die "no $type with $* came; the log has:
$(owner log --last 40 | cut -c1-300)"
}
field() { sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p" <<<"$2"; }

# The owner tells asker; asker's run sends, with its own command.
owner send asker 'Do it.
stand-in: use Bash {"command":"dispatcher send answerer from-asker-to-answerer"}' >/dev/null
asked="$(await message.sent '"agent":"asker"' '"to":"agent/answerer"')"
question="$(field id "$asked")"
[ -n "$question" ] || die "asker's message has no id: $asked"
step "asker's run wrote to answerer as itself (message $question, from a pod of asker's)"

# A run of answerer is started for it and answers.
answer="$(await message.sent '"agent":"answerer"' "\"answers\":\"$question\"")"
reply="$(field id "$answer")"
step "a run of answerer was started for it and answered (message $reply)"

# The answer goes back to asker's conversation, which is started again for it.
back="$(await message.delivered "$reply")"
grep -qF '"agent":"asker"' <<<"$back" || die "the answer was not delivered to asker: $back"
step "the answer reached asker: $(sed -n 's/.*\("how":"[a-z]*"\).*/\1/p' <<<"$back"), in a pod of asker's"

# With the stand-in every run's last word is an answer to the one before,
# so the two would answer each other for ever: the dispatcher ends an
# exchange at its sixth answer.
for _ in $(seq 60); do owner status | grep -q '^running: 0' && break; sleep 3; done
between="$(events message.sent | grep -c '"to":"session/' || true)"
[ "$between" -le 6 ] || die "the two agents' exchange went on to $between answers"
step "left to answer each other, the two stopped after $between answers: the dispatcher ends an exchange at six"

# Stranger is in nobody's writtenBy: its message to answerer is refused.
owner send stranger 'Do it.
stand-in: tell results
stand-in: use Bash {"command":"dispatcher send answerer from-a-stranger"}' >/dev/null
refused="$(await message.refused '"agent":"stranger"')"
grep -qF 'answerer' <<<"$refused" || die "the refusal is not of the message to answerer: $refused"
! events message.sent | grep -F '"agent":"stranger"' | grep -qF '"to":"agent/answerer"' || die "stranger's message to answerer was sent"
step "stranger's message to answerer was refused: $(field reason "$refused" | cut -c1-140)"
echo "a trial's agents write to each other, in the trial $(scripts/trial.sh "$context" name). Take it down with: scripts/trial.sh $context down"
