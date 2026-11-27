#!/usr/bin/env python3
"""Every arm of the task stream, so many times each, two arms at a time.

    all.py CONTEXT [--world stream] [--model MODEL] [--times 3] [--budget USD]

Each run is `scripts/stream.sh CONTEXT run ARM`, a trial of its own. Two
arms run side by side (four trials at once fill a sandbox's quota), a
round of the four arms after another. Before each pair the runs' own count
of what was spent so far is set against the budget, and nothing more is
started once it is passed. A run that stops to be gone on with (status 3)
ends this too, and says how to go on: `run ARM --resume DIR` for that run,
and this again for the rounds that are left.

The directories of the runs it started are listed in a file it names, for
`scripts/stream.sh CONTEXT report $(cat FILE)`.
"""
import json, os, re, subprocess, sys, time

context = sys.argv[1]
opts = {"--world": "stream", "--model": "claude-haiku-5-5", "--times": "3", "--budget": ""}
args = sys.argv[2:]
while args:
    if args[0] not in opts or len(args) < 2:
        sys.exit("stream: all takes --world, --model, --times and --budget")
    opts[args[0]] = args[1]
    args = args[2:]
root = os.path.join(os.environ.get("CARMAN_BENCH_ROOT") or os.path.expanduser("~/carman-bench"), "stream")
os.makedirs(root, exist_ok=True)
stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
listing = os.path.join(root, "all-%s-%s.txt" % (stamp, opts["--world"]))
dirs = []


def spent():
    """The runs' own count of what the runs started here cost."""
    total = 0.0
    for d in dirs:
        try:
            for line in open(d + "/tasks.jsonl"):
                total += json.loads(line).get("cost_usd", 0)
        except OSError:
            pass
        try:
            for line in open(d + "/run-usage.log"):
                m = re.search(r"\{.*\}", line)
                if m and '"agent":"dreamer"' in line:
                    total += json.loads(m.group(0)).get("cost_usd", 0)
        except OSError:
            pass
    return total


def where(log):
    """The directory of a run, from what its runner said."""
    found = re.findall(r"(?:results in|--resume) (\S+)", open(log).read())
    return found[-1] if found else ""


budget = float(opts["--budget"]) if opts["--budget"] else None
for round_ in range(1, int(opts["--times"]) + 1):
    for pair in (("none", "builtin"), ("notes", "tidied")):
        if budget is not None and spent() >= budget:
            sys.exit("stream: the runs so far cost $%.2f by their own count, which passes the budget of $%.2f: nothing more is started. What ran: %s" % (spent(), budget, listing))
        running = []
        for arm in pair:
            log = os.path.join(root, "all-%s-%d-%s.log" % (stamp, round_, arm))
            proc = subprocess.Popen(["scripts/stream.sh", context, "run", arm, "--world", opts["--world"], "--model", opts["--model"]],
                                    stdout=open(log, "w"), stderr=subprocess.STDOUT)
            running.append((arm, log, proc))
            time.sleep(20)
        stopped = False
        for arm, log, proc in running:
            code = proc.wait()
            d = where(log)
            if d:
                dirs.append(d)
                open(listing, "a").write(d + "\n")
            last = [l for l in open(log).read().splitlines() if "results in" in l or "stopped at" in l]
            print("stream: round %d, %s: status %d; %s" % (round_, arm, code, (last[-1] if last else "no word")[:220]), flush=True)
            stopped = stopped or code != 0
        print("stream: after round %d of %s: $%.2f by the runs' own count" % (round_, " and ".join(pair), spent()), flush=True)
        if stopped:
            sys.exit("stream: a run of round %d did not finish (see %s/all-%s-%d-*.log). Go on with a stopped one by: scripts/stream.sh %s run ARM --resume DIR; then start the rounds that are left. What ran: %s" % (round_, root, stamp, round_, context, listing))
print("stream: every run is done: %d runs, $%.2f by their own count. Their directories: %s" % (len(dirs), spent(), listing))
subprocess.call(["python3", "bench/stream/report.py"] + dirs)
