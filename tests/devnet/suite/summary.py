#!/usr/bin/env python3
"""Summarise a suite run, or compare it with a baseline run.

usage: summary.py RUN_DIR [BASELINE_DIR]

With a baseline (typically the same list at real cadence) it compares, test by
test, the verdict, the per-case verdicts (vault `CASE <id> PASS|FAIL` lines and
Go subtests, as a set, since best-effort helpers re-log a case on every retry),
and the wall time, and prints the speedup over the tests that passed in both.
"""
import collections
import os
import re
import sys


def load(run):
    path = os.path.join(run, "results.tsv")
    rows = {}
    with open(path) as f:
        next(f)
        for line in f:
            test, verdict, rc, start, secs, fast, log = line.rstrip("\n").split("\t")
            rows[test] = {"verdict": verdict, "secs": int(secs), "log": log}  # the last attempt wins
    return rows


def cases(log):
    found = set()
    if not os.path.exists(log):
        return found
    with open(log, errors="replace") as f:
        for line in f:
            if "NOT asserted" in line:
                continue
            m = re.search(r"CASE (\S+) (PASS|FAIL)", line)
            if m:
                found.add((m.group(1), m.group(2)))
            m = re.match(r"\s+--- (PASS|FAIL|SKIP): (\S+)", line)
            if m:
                found.add((m.group(2), m.group(1)))
    return found


def hours(secs):
    return f"{secs / 3600:.1f}h"


def main():
    if len(sys.argv) not in (2, 3):
        sys.exit(__doc__)
    run = load(sys.argv[1])
    counts = collections.Counter(r["verdict"] for r in run.values())
    print(f"{sys.argv[1]}: {len(run)} tests, {dict(counts)}, {hours(sum(r['secs'] for r in run.values()))}")
    if len(sys.argv) == 2:
        for test, r in run.items():
            if r["verdict"] != "PASS":
                print(f"  {r['verdict']:8} {test}")
        return

    base = load(sys.argv[2])
    both = [t for t in run if t in base]
    fast = real = 0
    n_same_cases = 0
    print(f"{'test':58} {'run':6} {'base':6} {'time':>7} {'base':>7} {'x':>5}  cases")
    for t in both:
        r, b = run[t], base[t]
        x = b["secs"] / r["secs"] if r["secs"] else 0
        same = cases(r["log"]) == cases(b["log"])
        n_same_cases += same
        if r["verdict"] == b["verdict"] == "PASS":
            fast += r["secs"]
            real += b["secs"]
        print(f"{t[:58]:58} {r['verdict']:6} {b['verdict']:6} {r['secs'] // 60:6}m {b['secs'] // 60:6}m {x:5.1f}  {'same' if same else 'DIFFER'}")
    print(f"\nverdict differs: {[t for t in both if run[t]['verdict'] != base[t]['verdict']]}")
    print(f"per-case verdicts identical on {n_same_cases} of {len(both)} tests")
    if fast:
        print(f"passed in both: {hours(fast)} vs {hours(real)}, {real / fast:.1f}x")


if __name__ == "__main__":
    main()
