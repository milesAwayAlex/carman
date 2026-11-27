#!/usr/bin/env python3
"""The runs of a stream, set side by side by arm.

    report.py DIR...        each a finished run of scripts/stream.sh

Every number is from what the agent did not write. Runs of one arm are put
together; a rate is of all their tasks.
"""
import json, sys
from collections import defaultdict

arms = defaultdict(list)  # arm -> [(meta, results)]
for d in sys.argv[1:]:
    try:
        meta = json.load(open(d + "/run.json"))
        results = json.load(open(d + "/results.json"))
    except OSError as err:
        sys.exit(f"stream: {d} is not a finished run: {err}")
    arms[meta["arm"]].append((meta, results))
if not arms:
    sys.exit("stream: name the runs' directories")
order = [a for a in ("none", "builtin", "notes", "tidied") if a in arms] + sorted(a for a in arms if a not in ("none", "builtin", "notes", "tidied"))


def rate(n, of):
    return "%d of %d (%d%%)" % (n, of, round(100 * n / of)) if of else "none"


def row(label, cell):
    print("%-34s %s" % (label, "  ".join("%-24s" % cell(a) for a in order)))


def every(arm):
    return [r for _, res in arms[arm] for r in res]


def pairs(arm, keep=lambda task, rule, l: True):
    """(task result, rule, lesson) for each rule a task turns on."""
    return [(r, rule, l) for r in every(arm) for rule, l in (r.get("lessons") or {}).items() if keep(r, rule, l)]


first = arms[order[0]][0]
print("world %s, model %s; runs: %s" % (first[0]["world"], first[0]["model"], ", ".join("%s %d" % (a, len(arms[a])) for a in order)))
row("", lambda a: a)
row("tasks passed", lambda a: rate(sum(r["pass"] for r in every(a)), len(every(a))))
for role in ("meet", "use", "told", "wrong", "neutral"):
    if any(r["role"] == role for r in every(order[0])):
        row("  " + role, lambda a: rate(sum(r["pass"] for r in every(a) if r["role"] == role), sum(1 for r in every(a) if r["role"] == role)))
days = sorted({r["day"] for r in every(order[0])})
for d in days:
    row("  day %d" % d, lambda a: rate(sum(r["pass"] for r in every(a) if r["day"] == d), sum(1 for r in every(a) if r["day"] == d)))
row("wrong calls a task", lambda a: "%.2f" % (sum(r["wrong_calls"] for r in every(a)) / max(1, len(every(a)))))
later = lambda r, rule, l: not l["met"]
row("a lesson needed again: followed", lambda a: rate(sum(l["used"] for _, _, l in pairs(a, later)), len(pairs(a, later))))
row("  refusals each time", lambda a: "%.2f" % (sum(l["refused"] for _, _, l in pairs(a, later)) / max(1, len(pairs(a, later)))))
row("  at its first meeting: refusals", lambda a: "%.2f" % (sum(l["refused"] for _, _, l in pairs(a, lambda r, rule, l: l["met"])) / max(1, len(pairs(a, lambda r, rule, l: l["met"])))))
row("  had it and followed it", lambda a: rate(sum(1 for _, _, l in pairs(a, later) if l["had"] and l["used"]), len(pairs(a, later))))
row("  had it, did not follow", lambda a: rate(sum(1 for _, _, l in pairs(a, later) if l["had"] and not l["used"]), len(pairs(a, later))))
row("  did not have it, followed", lambda a: rate(sum(1 for _, _, l in pairs(a, later) if not l["had"] and l["used"]), len(pairs(a, later))))
row("  did not have it, did not", lambda a: rate(sum(1 for _, _, l in pairs(a, later) if not l["had"] and not l["used"]), len(pairs(a, later))))


def refused(a, *names):
    return sum(n for r in every(a) for name, n in (r.get("refused") or {}).items() if name in names)


row("harm: an old rule followed", lambda a: "%d calls" % refused(a, "cr-old", "approval-old"))
row("harm: a wrong note followed", lambda a: "%d calls" % refused(a, "force", "approval-wrong"))
row("harm: neutral tasks failed", lambda a: rate(sum(1 for r in every(a) if r["role"] == "neutral" and not r["pass"]), sum(1 for r in every(a) if r["role"] == "neutral")))
cost = lambda a: sum(r.get("cost_usd", 0) for r in every(a)) + sum(m.get("nights_cost_usd", 0) for m, _ in arms[a])
row("cost, runs' own count", lambda a: "$%.4f" % cost(a))
row("  of which the nights", lambda a: "$%.4f" % sum(m.get("nights_cost_usd", 0) for m, _ in arms[a]))
row("  a task", lambda a: "$%.4f" % (cost(a) / max(1, len(every(a)))))
row("  a passed task", lambda a: "$%.4f" % (cost(a) / max(1, sum(r["pass"] for r in every(a)))))
row("notes written", lambda a: "%d" % sum(r.get("notes", 0) for r in every(a)))
row("recalls", lambda a: "%d" % sum(r.get("recalls", 0) for r in every(a)))
row("entries ever in memory, a run", lambda a: "%.0f" % (sum(m.get("entries_ever", 0) for m, _ in arms[a]) / len(arms[a])))
row("minutes a run", lambda a: "%.1f" % (sum(m.get("seconds", 0) for m, _ in arms[a]) / len(arms[a]) / 60))

rules = sorted({rule for a in order for _, rule, _ in pairs(a)})
if rules:
    print("\nby rule, where it is needed again: followed (refusals each time)")
    for rule in rules:
        this = lambda r, name, l, rule=rule: name == rule and not l["met"]
        row("  " + rule, lambda a: "%s (%.2f)" % (rate(sum(l["used"] for _, _, l in pairs(a, this)), len(pairs(a, this))), sum(l["refused"] for _, _, l in pairs(a, this)) / max(1, len(pairs(a, this)))))
if len(sys.argv[1:]) <= 4:
    print("\nby task: pass or fail, wrong calls")
    for i, r in enumerate(every(order[0])[: len(arms[order[0]][0][1])]):
        cells = []
        for a in order:
            cells.append(" ".join("%s%d" % ("p" if res[i]["pass"] else "F", res[i]["wrong_calls"]) for _, res in arms[a]))
        print("  %-5s day %d %-8s %s" % (r["task"], r["day"], r["role"], "  ".join("%-24s" % c for c in cells)))
