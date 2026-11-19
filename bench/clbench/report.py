#!/usr/bin/env python3
"""Summarise task-stream benchmark results, one line per run.

    python3 bench/clbench/report.py [TASK]        (default: database_exploration)

Reads the benchmark's saved results under .local/clbench/results/TASK. Needs
only the Python standard library. Runs that failed are listed with the reason,
so a missing arm is never mistaken for a low score.
"""

from __future__ import annotations

import gzip
import json
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]


def mean(xs):
    xs = list(xs)
    return sum(xs) / len(xs) if xs else float("nan")


def summarise(trace: dict) -> dict:
    outcomes = trace.get("instance_outcomes") or []
    interactions = trace.get("interactions") or []
    half = len(outcomes) // 2
    tokens_in = tokens_out = cost = 0.0
    for step in interactions:
        usage = (step.get("usage") or {}).get("respond") or {}
        tokens_in += usage.get("input_tokens", 0) or 0
        tokens_out += usage.get("output_tokens", 0) or 0
    for outcome in outcomes:
        cost += outcome.get("cost_usd") or 0
    correct = [o for o in outcomes if o.get("success")]
    return {
        "score": (trace.get("result") or {}).get("score", float("nan")),
        "questions": len(outcomes),
        "correct": len(correct),
        "first_half": mean(o.get("reward", 0) for o in outcomes[:half]),
        "second_half": mean(o.get("reward", 0) for o in outcomes[half:]),
        "queries": mean((o.get("metadata") or {}).get("num_queries", 0) for o in outcomes),
        "queries_when_correct": mean((o.get("metadata") or {}).get("num_queries", 0) for o in correct),
        "calls": len(interactions),
        "input_mtok": tokens_in / 1e6,
        "output_ktok": tokens_out / 1e3,
        "cost": cost,
        "seconds": mean(o.get("latency_seconds") or 0 for o in outcomes),
    }


def main() -> int:
    task = sys.argv[1] if len(sys.argv) > 1 else "database_exploration"
    root = REPO / ".local" / "clbench" / "results" / task
    if not root.is_dir():
        print(f"no results at {root}", file=sys.stderr)
        return 1

    rows = []
    for path in sorted(root.glob("viewer_artifact_*.json.gz")):
        with gzip.open(path, "rt") as fh:
            data = json.load(fh)
        arm = data["summary"]["system"]["name"]
        for run in data.get("run_traces") or []:
            rows.append((arm, path.name.split("_")[2], run.get("run_index", 0), summarise(run["trace"])))

    header = (
        f"{'arm':<17}{'started (UTC)':<29}{'run':>3}  {'score':>6} {'correct':>8} {'1st half':>9} {'2nd half':>9} "
        f"{'queries/q':>10} {'q if right':>10} {'calls':>6} {'in Mtok':>8} {'out ktok':>9} {'est $':>6} {'s/question':>10}"
    )
    print(header)
    for arm, started, index, s in sorted(rows):
        print(
            f"{arm:<17}{started:<29}{index:>3}  {s['score']:>6.3f} {s['correct']:>4}/{s['questions']:<3} "
            f"{s['first_half']:>9.3f} {s['second_half']:>9.3f} {s['queries']:>10.2f} {s['queries_when_correct']:>10.2f} "
            f"{s['calls']:>6} {s['input_mtok']:>8.2f} {s['output_ktok']:>9.0f} {s['cost']:>6.2f} {s['seconds']:>10.1f}"
        )

    print("\nmean score per arm:")
    for arm in sorted({r[0] for r in rows}):
        scores = [r[3]["score"] for r in rows if r[0] == arm]
        spread = f", range {min(scores):.3f} to {max(scores):.3f}" if len(scores) > 1 else ""
        print(f"  {arm:<17} {mean(scores):.3f} over {len(scores)} run(s){spread}")

    failed = sorted(root.glob("failed_*.json"))
    if failed:
        print("\nruns that failed (not scored):")
        for path in failed:
            data = json.loads(path.read_text())
            arm = ((data.get("system_artifacts") or {}).get("carman_arm")) or "?"
            message = ((data.get("error") or {}).get("error_message") or "").replace("\n", " ")
            print(f"  {arm:<17} {path.name.split('_')[3]}  {message[:150]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
