#!/usr/bin/env bash
# Checks, without a cluster, that the nightly backup keeps a standing agent's
# sessions and the jobs' kept conversations so that they can be put back:
# makes small trees shaped like an agent's home and like the conversations'
# volume, copies them with backup-state and the commands the chart gives it,
# into a restic repository in a directory, restores the copies elsewhere and
# compares. Needs restic and python3; with helm, the commands are the chart's own.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v restic >/dev/null || { echo "check-backup-copy: restic is not here; skipped" >&2; exit 0; }

t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT
mnt="$t/mnt" home="$t/mnt/homes/desk" kept="$t/mnt/conversations"
fail() { echo "check-backup-copy: $*" >&2; exit 1; }

# A home: what a session needs, and what is left out.
slug=-home-agent-work-repo id=11111111-2222-4333-8444-555555555555
mkdir -p "$home/.claude/projects/$slug/$id/subagents" "$home/.claude/todos" "$home/.config/agent" \
    "$home/work/repo/.git" "$home/.cache/big" "$home/.claude/plugins" "$home/.claude/sessions"
# Big enough for restic to cut it in several pieces, and not compressible.
python3 -c 'import hashlib
for i in range(1, 2001):
    print("{\"type\":\"user\",\"n\":%d,\"text\":\"%s\"}" % (i, hashlib.sha512(str(i).encode()).hexdigest() * 32))' > "$home/.claude/projects/$slug/$id.jsonl"
# A line being written as the copy is taken: cut off mid-way.
printf '{"type":"assistant","n":2001,"half' >> "$home/.claude/projects/$slug/$id.jsonl"
echo '{"type":"user"}' > "$home/.claude/projects/$slug/$id/subagents/agent-1.jsonl"
: > "$home/.claude/projects/$slug/$id.lock"
echo '[]' > "$home/.claude/todos/$id.json"
echo '{"projects":{"/home/agent/work":{"hasTrustDialogAccepted":true}}}' > "$home/.claude.json"
echo '{"permissions":{}}' > "$home/.claude/settings.json"
echo '{"token":"not to be copied"}' > "$home/.claude/.credentials.json"
printf 'repo\t/home/agent/work/repo\n' > "$home/.config/agent/sessions"
echo 'uncommitted' > "$home/work/repo/notes.txt"
echo cache > "$home/.cache/big/blob"
echo '{"pid":1}' > "$home/.claude/sessions/1.json"
# The conversations' volume: by agent, then directory, then conversation.
mkdir -p "$kept/dreamer/-work" && cp "$home/.claude/projects/$slug/$id.jsonl" "$kept/dreamer/-work/$id.jsonl"
: > "$kept/dreamer/-work/$id.lock"

# The commands the backup runs, as the chart renders them for one standing
# agent, with the volumes where this check put them.
if command -v helm >/dev/null; then
    also="$(helm template t charts/carman --set memory.image=m:0 --set agent.image=a:0 --set dispatcher.image=d:0 \
        --set dispatcher.enabled=false --set backup.enabled=true --set backup.repository=r --set agent.sessionStorage=1Gi \
        --set-json 'agents=[{"name":"desk","interactive":true}]' --show-only templates/backup.yaml |
        sed -n '/- name: BACKUP_ALSO/{n;s/^ *value: //p}' | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()))')"
else
    echo "check-backup-copy: helm is not here; the commands are this script's copy of the chart's" >&2
    also="$(python3 -c 'import json,sys; print(json.dumps([
        {"name": "conversations", "command": ["backup-state", "tree", "/var/lib/backup/conversations"]},
        {"name": "sessions-desk", "command": ["backup-state", "tree", "/var/lib/backup/homes/desk"] + sys.argv[1:]}]))' \
        $(sed -n '/^  sessionPaths:/,/^  [a-z]/s/^    - //p' charts/carman/values.yaml))"
fi
also="${also//\/var\/lib\/backup/$mnt}"
echo "$also" | grep -q '"sessions-desk"' || fail "the chart does not copy the standing agent's sessions: $also"
echo "$also" | grep -q '"conversations"' || fail "the chart does not copy the kept conversations: $also"

export PATH="$PWD/deploy/agent:$PATH" BACKUP_REPOSITORY="$t/repo" RESTIC_PASSWORD=check RESTIC_CACHE_DIR="$t/cache"
unset CMEM_URL BACKUP_KEEP
BACKUP_ALSO="$also" backup-state > "$t/log" 2>&1 || { cat "$t/log" >&2; fail "the backup failed"; }
backup-state list | grep -q sessions-desk || fail "list does not show sessions-desk"

mkdir -p "$t/back/home" "$t/back/kept"
backup-state dump sessions-desk | tar -x -C "$t/back/home"
backup-state dump conversations | tar -x -C "$t/back/kept"

# What came back is what was there, less what is left out, and each
# transcript cut at its last whole line.
want="$t/want"
mkdir -p "$want"
(cd "$home" && cp -a --parents .claude/projects .claude/todos .claude/settings.json .claude.json .config/agent/sessions "$want/")
cp -a "$kept" "$want/kept"
find "$want" -name '*.lock' -delete
for f in "$want/.claude/projects/$slug/$id.jsonl" "$want/kept/dreamer/-work/$id.jsonl"; do
    python3 - "$f" <<'PY'
import sys
data = open(sys.argv[1], "rb").read()
open(sys.argv[1], "wb").write(data[: data.rfind(b"\n") + 1])
PY
done
diff -r "$want/kept" "$t/back/kept" || fail "the kept conversations did not come back as they were"
rm -rf "$want/kept"
diff -r "$want" "$t/back/home" || fail "the agent's sessions did not come back as they were"
python3 -c 'import json,sys; [json.loads(l) for l in open(sys.argv[1])]' "$t/back/home/.claude/projects/$slug/$id.jsonl" ||
    fail "a restored transcript has a line that is not whole"
[ "$(wc -l < "$t/back/home/.claude/projects/$slug/$id.jsonl")" -eq 2000 ] || fail "the restored transcript lost whole lines"

# The next night, with a line added: the stream is in the same order, so
# restic stores the new end, not the transcript again.
before="$(du -sk "$t/repo" | cut -f1)"
printf '"}\n' >> "$home/.claude/projects/$slug/$id.jsonl"
BACKUP_ALSO="$also" backup-state > "$t/log" 2>&1 || { cat "$t/log" >&2; fail "the second backup failed"; }
grew="$(( $(du -sk "$t/repo" | cut -f1) - before ))"
size="$(du -sk "$home/.claude/projects/$slug/$id.jsonl" | cut -f1)"
[ "$grew" -lt "$((size / 2))" ] || fail "the second night stored ${grew} kB for a transcript of ${size} kB: the copy is not stored as a change"
echo "check-backup-copy: sessions and kept conversations copied, restored and compared; the next night stored ${grew} kB for a ${size} kB transcript"
