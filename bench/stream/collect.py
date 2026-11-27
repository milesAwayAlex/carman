#!/usr/bin/env python3
"""What one run of one arm showed, put together from what the agent did not
write: the world's score, the memory service's log, the runs' own usage.

    collect.py OUT ARM WORLD MODEL BEGAN TASKS

Reads OUT/score.json, OUT/state.json, OUT/tasks.jsonl, OUT/ID.memory.log,
OUT/run-usage.log and the library under OUT/memory; writes OUT/results.json
and OUT/run.json.
"""
import glob, json, os, re, sys, time

out, arm, world, model, began, tasks_file = sys.argv[1:7]
score = json.load(open(out + "/score.json"))
state = json.load(open(out + "/state.json"))
tasks = {t["id"]: t for t in json.load(open(tasks_file))}

# Every entry memory ever held in this run, by id: its text.
text = {}
for f in glob.glob(out + "/memory/library/entries/**/*.md", recursive=True):
    text[os.path.basename(f)[:-3]] = open(f).read().split("\n---\n", 1)[-1]

# The command a rule governs: the lesson has to be there before its first call.
GOVERNS = {"change": "deploy", "cr": "deploy", "image": "deploy", "staged": "deploy", "approval": "deploy",
           "registry": "registry", "reason": "config", "cap": "config", "name": "service",
           "reviewed": "attach", "summary": "attach"}
# What says a told rule was followed: the check on what was attached.
TOLD = {"reviewed": "release-note", "summary": "summary"}


def stamp(s):
    s = s.rstrip("Z")
    head, _, frac = s.partition(".")
    return time.mktime(time.strptime(head, "%Y-%m-%dT%H:%M:%S")) + float("0." + (frac or "0"))


def memory_log(task):
    """(time, kind, ids) for each line of the memory service's log in the task."""
    rows = []
    try:
        lines = open(f"{out}/{task}.memory.log").read().splitlines()
    except OSError:
        return rows
    for line in lines:
        kind = re.search(r" msg=(\w+) ", line)
        when = re.search(r"time=(\S+)", line)
        if not kind or not when:
            continue
        ids = []
        for key in ("shown", "found"):
            m = re.search(key + r'="?\[([^\]]*)\]', line)
            ids += m.group(1).split() if m else []
        m = re.search(r" entry=(\S+)", line)
        ids += [m.group(1)] if m else []
        rows.append((stamp(when.group(1)), kind.group(1), ids))
    return rows


extra = {}
for line in open(out + "/tasks.jsonl"):
    row = json.loads(line)
    extra[row["task"]] = row
for r in score:
    tid = r["task"]
    r.update({k: v for k, v in extra.get(tid, {}).items() if k != "task"})
    log = memory_log(tid)
    wrote = {i for _, kind, ids in log if kind == "note" for i in ids}
    given = [(t, i) for t, kind, ids in log if kind in ("index", "flashback", "recall", "read") for i in ids if i not in wrote]
    r["given"] = sorted({i for _, i in given})
    r["wrote"] = sorted(wrote)
    r["recalls"] = sum(1 for _, kind, _ in log if kind == "recall")
    r["notes"] = len(wrote)
    calls = [c for c in state["calls"] if c["task"] == tid]
    failed = set(r.get("failed") or [])
    lessons = {}
    for rule in r.get("needs") or []:
        pattern = (tasks[tid].get("lessons") or {}).get(rule, "")
        verb = GOVERNS.get(rule, "")
        first = next((stamp(c["at"]) for c in calls if c["argv"] and c["argv"][0] == verb), None)
        # Had it: an entry holding the lesson, not written by this session,
        # was given to it before its first call of the command the rule governs.
        had = any(re.search(pattern, text.get(i, ""), re.I | re.S) and (first is None or t < first) for t, i in given) if pattern else False
        refused = sum(n for name, n in (r.get("refused") or {}).items() if name == rule or name.startswith(rule + "-"))
        used = (TOLD[rule] not in failed) if rule in TOLD else refused == 0
        lessons[rule] = {"had": had, "used": used, "refused": refused, "met": rule in (r.get("met") or [])}
    if lessons:
        r["lessons"] = lessons
json.dump(score, open(out + "/results.json", "w"), indent=1)

nights = 0.0
try:
    for line in open(out + "/run-usage.log"):
        m = re.search(r"\{.*\}", line)
        if m and '"agent":"dreamer"' in line:
            nights += json.loads(m.group(0)).get("cost_usd", 0)
except OSError:
    pass
json.dump({"arm": arm, "world": world, "model": model, "seconds": int(time.time()) - int(began),
           "nights_cost_usd": nights, "entries_ever": len(text)}, open(out + "/run.json", "w"))
