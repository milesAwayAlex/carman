#!/usr/bin/env python3
"""Whether a task's run stopped at the plan's usage limit, and how long to
wait before trying it again.

    dispatcher message ID | limit.py [NOW]

Reads what the dispatcher says became of the message a task was sent as
(its events, among them message.limited with when the limit resets, ADR
0067) and prints one line:

    none            the run did not stop at the plan's limit
    wait SECONDS    it did; the limit resets in SECONDS less two minutes
    wait-unknown    it did, and when the limit resets was not said

NOW is the time to count from (RFC 3339), for a test.
"""
import json, re, sys, time, calendar


def stamp(s):
    return calendar.timegm(time.strptime(s[:19], "%Y-%m-%dT%H:%M:%S"))


def decide(text, now):
    for line in text.splitlines():
        if "message.limited" not in line:
            continue
        m = re.search(r"\{.*\}", line)
        resets = ""
        if m:
            try:
                resets = json.loads(m.group(0)).get("resets", "")
            except ValueError:
                pass
        if not resets or resets.startswith("0001"):
            return "wait-unknown"
        # Two minutes after the reset: the window is the plan's clock, not ours.
        return "wait %d" % max(60, stamp(resets) - now + 120)
    return "none"


if __name__ == "__main__":
    now = stamp(sys.argv[1]) if len(sys.argv) > 1 else int(time.time())
    print(decide(sys.stdin.read(), now))
