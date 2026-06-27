#!/usr/bin/env python3
"""
aggregate_repeats.py - aggregate repeated benchmark runs into mean/median/std/95% CI.

Consumes a root produced by bench/repeat_study.sh:
    bench_results/repeats_<label>/rep_1/{connection,latency,scale,throughput}/*.csv
    bench_results/repeats_<label>/rep_2/...
and writes tidy aggregates to:
    bench_results/repeats_<label>/aggregated/{connection,latency,scale,throughput}.csv
plus a combined summary.csv.

Pure standard library (csv, glob, math, statistics) - no numpy needed.

Usage:
    python3 bench/aggregate_repeats.py bench_results/repeats_baseline
"""
import csv
import glob
import math
import os
import statistics
import sys
from collections import defaultdict

# Two-sided 95% Student-t critical values by degrees of freedom (n-1).
T95 = {1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447,
       7: 2.365, 8: 2.306, 9: 2.262, 10: 2.228, 12: 2.179, 15: 2.131,
       20: 2.086, 25: 2.060, 30: 2.042}


def t95(df):
    if df <= 0:
        return 0.0
    if df in T95:
        return T95[df]
    if df > 30:
        return 1.96
    # nearest known df below
    keys = sorted(k for k in T95 if k <= df)
    return T95[keys[-1]] if keys else 1.96


def fnum(row, key):
    v = row.get(key, "")
    if v is None or v == "":
        return None
    try:
        return float(v)
    except ValueError:
        return None


# samples[(dimension, proto, param_name, param_value, metric)] = [values...]
samples = defaultdict(list)


def add(dim, proto, pname, pval, metric, value):
    if value is not None:
        samples[(dim, proto, pname, str(pval), metric)].append(value)


def collect_connection(repdir):
    for f in glob.glob(os.path.join(repdir, "connection", "dial_*.csv")):
        with open(f) as fh:
            for row in csv.DictReader(fh):
                p, n = row.get("proto"), row.get("n")
                if not p:
                    continue
                for m in ("dial_ms_avg", "dial_ms_p50", "dial_ms_p95"):
                    add("connection", p, "n", n, m, fnum(row, m))
                add("connection", p, "n", n, "errors_total", fnum(row, "errors_total"))


def collect_latency(repdir):
    for f in glob.glob(os.path.join(repdir, "latency", "latency_*.csv")):
        with open(f) as fh:
            for row in csv.DictReader(fh):
                p, size = row.get("proto"), row.get("size")
                if not p:
                    continue
                for m in ("rtt_p50", "rtt_p95", "rtt_min", "rtt_max"):
                    add("latency", p, "size", size, m, fnum(row, m))
                sent, recv = fnum(row, "msgs_sent"), fnum(row, "msgs_recv")
                if sent and sent > 0 and recv is not None:
                    add("latency", p, "size", size, "delivery_pct", 100.0 * recv / sent)


def collect_scale(repdir):
    for f in glob.glob(os.path.join(repdir, "scale", "scale_*.csv")):
        with open(f) as fh:
            for row in csv.DictReader(fh):
                p, n = row.get("proto"), row.get("n")
                if not p:
                    continue
                for m in ("p50_rtt_ms", "p95_rtt_ms", "dial_ms_avg"):
                    add("scale", p, "n", n, m, fnum(row, m))
                add("scale", p, "n", n, "errors_total", fnum(row, "errors_total"))
                sent, recv = fnum(row, "total_msgs_sent"), fnum(row, "total_msgs_recv")
                if sent and sent > 0 and recv is not None:
                    add("scale", p, "n", n, "delivery_pct", 100.0 * recv / sent)


def collect_throughput(repdir):
    for f in glob.glob(os.path.join(repdir, "throughput", "throughput_*.csv")):
        with open(f) as fh:
            for row in csv.DictReader(fh):
                p, rate = row.get("proto"), row.get("rate")
                if not p:
                    continue
                add("throughput", p, "rate", rate, "avg_throughput_msgs_per_sec",
                    fnum(row, "avg_throughput_msgs_per_sec"))
                add("throughput", p, "rate", rate, "errors_total", fnum(row, "errors_total"))
                sent, recv = fnum(row, "msgs_sent"), fnum(row, "msgs_recv")
                if sent and sent > 0 and recv is not None:
                    add("throughput", p, "rate", rate, "delivery_pct", 100.0 * recv / sent)


def summarise():
    rows = []
    for (dim, proto, pname, pval, metric), vals in samples.items():
        n = len(vals)
        mean = statistics.fmean(vals)
        med = statistics.median(vals)
        sd = statistics.stdev(vals) if n > 1 else 0.0
        half = t95(n - 1) * sd / math.sqrt(n) if n > 1 else 0.0
        rows.append({
            "dimension": dim, "proto": proto, "param": pname,
            "value": pval, "metric": metric, "n_reps": n,
            "mean": round(mean, 3), "median": round(med, 3),
            "stddev": round(sd, 3), "ci95_halfwidth": round(half, 3),
            "ci95_low": round(mean - half, 3), "ci95_high": round(mean + half, 3),
        })

    def sort_key(r):
        try:
            v = float(r["value"])
        except ValueError:
            v = 0.0
        return (r["dimension"], r["proto"], v, r["metric"])
    rows.sort(key=sort_key)
    return rows


def main():
    if len(sys.argv) < 2:
        print("usage: python3 bench/aggregate_repeats.py bench_results/repeats_<label>")
        sys.exit(1)
    root = sys.argv[1].rstrip("/")
    repdirs = sorted(glob.glob(os.path.join(root, "rep_*")))
    if not repdirs:
        print(f"no rep_* dirs under {root}")
        sys.exit(1)
    print(f"aggregating {len(repdirs)} repeats under {root}")
    for rd in repdirs:
        collect_connection(rd)
        collect_latency(rd)
        collect_scale(rd)
        collect_throughput(rd)

    rows = summarise()
    outdir = os.path.join(root, "aggregated")
    os.makedirs(outdir, exist_ok=True)
    cols = ["dimension", "proto", "param", "value", "metric", "n_reps",
            "mean", "median", "stddev", "ci95_halfwidth", "ci95_low", "ci95_high"]

    # per-dimension files + combined
    with open(os.path.join(outdir, "summary.csv"), "w", newline="") as fh:
        w = csv.DictWriter(fh, fieldnames=cols)
        w.writeheader()
        w.writerows(rows)
    for dim in ("connection", "latency", "scale", "throughput"):
        drows = [r for r in rows if r["dimension"] == dim]
        if not drows:
            continue
        with open(os.path.join(outdir, f"{dim}.csv"), "w", newline="") as fh:
            w = csv.DictWriter(fh, fieldnames=cols)
            w.writeheader()
            w.writerows(drows)

    # human-readable highlights
    print(f"\nwrote {outdir}/summary.csv ({len(rows)} rows)\n")
    print("highlights (mean +/- 95% CI):")
    want = {
        ("connection", "dial_ms_avg"), ("latency", "rtt_p50"),
        ("scale", "delivery_pct"), ("scale", "p50_rtt_ms"),
        ("throughput", "delivery_pct"),
    }
    for r in rows:
        if (r["dimension"], r["metric"]) in want:
            print(f"  {r['dimension']:<11} {r['proto']:<4} "
                  f"{r['param']}={r['value']:<5} {r['metric']:<26} "
                  f"{r['mean']:>9.3f} +/- {r['ci95_halfwidth']:<7.3f} (n={r['n_reps']})")


if __name__ == "__main__":
    main()
