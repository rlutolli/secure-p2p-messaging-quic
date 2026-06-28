#!/usr/bin/env python3
"""
chart_repeats.py - publication figures (with 95% CI error bars) from the repeated-trial
aggregates. Outputs PNGs into report/figures/ (prefix res_) for the dissertation.

Inputs (produced by bench/aggregate_repeats.py):
  bench_results/repeats_baseline/aggregated/summary.csv
  bench_results/repeats_improved/aggregated/summary.csv
  bench_results/repeats_loss1/aggregated/summary.csv
  bench_results/repeats_loss3/aggregated/summary.csv
  bench_results/repeats_multistream_wan/multistream.csv

fig_0rtt() reads from bench_results/0rtt_results.csv (produced by raw_mini/quic_0rtt.go)
if present; otherwise it uses placeholder values and notes this in the chart title.

Usage: python3 bench/chart_repeats.py
"""

import csv, math, os, statistics
from collections import defaultdict
import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

OUT = "report/figures"
os.makedirs(OUT, exist_ok=True)
QC, TC = "#1f77b4", "#d62728"  # QUIC blue, TCP red


def load(p):
    d = {}
    try:
        with open(p) as f:
            for r in csv.DictReader(f):
                d[(r["dimension"], r["proto"], r["metric"], r["value"])] = (
                    float(r["mean"]),
                    float(r["ci95_halfwidth"]),
                )
    except FileNotFoundError:
        pass
    return d


base = load("bench_results/repeats_baseline/aggregated/summary.csv")
imp = load("bench_results/repeats_improved/aggregated/summary.csv")
l1 = load("bench_results/repeats_loss1/aggregated/summary.csv")
l3 = load("bench_results/repeats_loss3/aggregated/summary.csv")


def series(d, dim, proto, metric, xs):
    ys, es = [], []
    for x in xs:
        k = (dim, proto, metric, str(x))
        if k in d:
            ys.append(d[k][0])
            es.append(d[k][1])
        else:
            ys.append(float("nan"))
            es.append(0)
    return ys, es


# ── Fig 1: scale broadcast RTT, baseline vs improved ─────────────────────────
def fig_scale():
    xs = [10, 50, 200, 500, 1000]
    X = list(range(len(xs)))
    fig, ax = plt.subplots(figsize=(7, 4.3))
    for d, lab, c, ls in [
        (base, "QUIC baseline", QC, "--"),
        (imp, "QUIC improved", QC, "-"),
        (base, "TCP baseline", TC, "--"),
        (imp, "TCP improved", TC, "-"),
    ]:
        proto = "quic" if "QUIC" in lab else "tcp"
        ys, es = series(d, "scale", proto, "p50_rtt_ms", xs)
        ax.errorbar(
            X, ys, yerr=es, marker="o", capsize=3, color=c, linestyle=ls, label=lab
        )
    ax.set_xticks(X)
    ax.set_xticklabels(xs)
    ax.set_xlabel("Concurrent peers (n)")
    ax.set_ylabel("Broadcast RTT p50 (ms)")
    ax.set_title(
        "Scale: broadcast RTT vs peers — baseline vs improved relay (mean ± 95% CI, n=5)"
    )
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_scale_rtt_improvement.png", dpi=150)
    plt.close(fig)


# ── Fig 2: multistream total elapsed ─────────────────────────────────────────
def fig_multistream():
    d = defaultdict(list)
    try:
        with open("bench_results/repeats_multistream_wan/multistream.csv") as f:
            for r in csv.DictReader(f):
                d[(r["proto"], int(r["streams"]))].append(float(r["total_elapsed_ms"]))
    except FileNotFoundError:
        return
    xs = [4, 8, 16, 32]
    X = list(range(len(xs)))
    fig, ax = plt.subplots(figsize=(7, 4.3))
    for proto, c in [("quic", QC), ("tcp", TC)]:
        ys, es = [], []
        for n in xs:
            v = d[(proto, n)]
            m = statistics.fmean(v)
            sd = statistics.stdev(v) if len(v) > 1 else 0
            ys.append(m)
            es.append(2.776 * sd / math.sqrt(len(v)) if len(v) > 1 else 0)
        ax.errorbar(X, ys, yerr=es, marker="s", capsize=3, color=c, label=proto.upper())
    ax.set_xticks(X)
    ax.set_xticklabels(xs)
    ax.set_xlabel("Parallel streams / connections (N)")
    ax.set_ylabel("Total elapsed (ms)")
    ax.set_title(
        "Multistream: QUIC N streams (1 conn) vs TCP N connections (mean ± 95% CI, n=5)"
    )
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_multistream.png", dpi=150)
    plt.close(fig)


# ── Fig 3: connection dial vs n ──────────────────────────────────────────────
def fig_connection():
    xs = [10, 50, 100, 200, 500]
    X = list(range(len(xs)))
    fig, ax = plt.subplots(figsize=(7, 4.3))
    for proto, c in [("quic", QC), ("tcp", TC)]:
        ys, es = series(base, "connection", proto, "dial_ms_avg", xs)
        ax.errorbar(X, ys, yerr=es, marker="o", capsize=3, color=c, label=proto.upper())
    ax.set_xticks(X)
    ax.set_xticklabels(xs)
    ax.set_xlabel("Concurrent peers (n)")
    ax.set_ylabel("Dial time avg (ms)")
    ax.set_ylim(bottom=0)
    ax.set_title(
        "Connection setup: QUIC (1-RTT) vs TCP+TLS (~2-RTT) (mean ± 95% CI, n=5)"
    )
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_connection_dial.png", dpi=150)
    plt.close(fig)


# ── Fig 4: latency p50 by size (clean) ───────────────────────────────────────
def fig_latency():
    xs = [64, 256, 1024, 4096, 16384, 65536]
    X = list(range(len(xs)))
    fig, ax = plt.subplots(figsize=(7, 4.3))
    for proto, c in [("quic", QC), ("tcp", TC)]:
        ys, es = series(base, "latency", proto, "rtt_p50", xs)
        ax.errorbar(X, ys, yerr=es, marker="o", capsize=3, color=c, label=proto.upper())
    ax.set_xticks(X)
    ax.set_xticklabels(["64", "256", "1K", "4K", "16K", "64K"])
    ax.set_xlabel("Payload size (bytes)")
    ax.set_ylabel("Message RTT p50 (ms)")
    ax.set_title("Latency by payload size, WAN clean path (mean ± 95% CI, n=5)")
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_latency_by_size.png", dpi=150)
    plt.close(fig)


# ── Fig 5: loss p95 by size, clean vs 3% ─────────────────────────────────────
def fig_loss():
    xs = [64, 1024, 16384, 65536]
    X = list(range(len(xs)))
    fig, ax = plt.subplots(figsize=(7.5, 4.3))
    combos = [
        (base, "quic", "QUIC 0%", QC, "--"),
        (l3, "quic", "QUIC 3%", QC, "-"),
        (base, "tcp", "TCP 0%", TC, "--"),
        (l3, "tcp", "TCP 3%", TC, "-"),
    ]
    for d, proto, lab, c, ls in combos:
        ys, es = series(d, "latency", proto, "rtt_p95", xs)
        ax.errorbar(
            X, ys, yerr=es, marker="^", capsize=3, color=c, linestyle=ls, label=lab
        )
    ax.set_xticks(X)
    ax.set_xticklabels(["64", "1K", "16K", "64K"])
    ax.set_xlabel("Payload size (bytes)")
    ax.set_ylabel("Message RTT p95 (ms)")
    ax.set_title(
        "Loss resilience: tail latency (p95), clean vs 3% loss (mean ± 95% CI, n=3)"
    )
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_loss_p95.png", dpi=150)
    plt.close(fig)


# ── Fig 6: 0-RTT reconnect dial (read from CSV) ────────────────────────────
def fig_0rtt():
    fig, ax = plt.subplots(figsize=(7.0, 4.3))

    # Try to load real data; fall back to placeholder
    csv_path = "bench_results/0rtt_results.csv"
    data = None
    try:
        with open(csv_path) as f:
            rows = list(csv.DictReader(f))

        # Build 3-bar chart: 1-RTT handshake_complete, 0-RTT handshake_complete,
        # and 0-RTT first_app_data_sent (the only true client-side advantage).
        # Convert microseconds to milliseconds.
        def get(proto, metric):
            for r in rows:
                if r["proto"] == proto and r["metric"] == metric:
                    return float(r["mean_us"]) / 1000.0, float(
                        r["ci95_halfwidth_us"]
                    ) / 1000.0
            return None, None

        q1_mean, q1_ci = get("1-RTT", "handshake_complete")
        q0_mean, q0_ci = get("0-RTT", "handshake_complete")
        # For the 0-RTT, also show the "first_app_data_sent" — that's the only true advantage
        q0_adv_mean, q0_adv_ci = get("0-RTT", "first_app_data_sent")

        if q1_mean is not None and q0_mean is not None and q0_adv_mean is not None:
            # Real data: 3 bars — QUIC 1-RTT handshake, QUIC 0-RTT handshake,
            # QUIC 0-RTT first-app-data (the ONLY true client-side advantage)
            labels = [
                "QUIC\n1-RTT\n(handshake)",
                "QUIC\n0-RTT\n(handshake)",
                "QUIC\n0-RTT\n(first app-data sent)",
            ]
            vals = [q1_mean, q0_mean, q0_adv_mean]
            errs = [q1_ci, q0_ci, q0_adv_ci]
            colors = [QC, QC, "#2ca02c"]  # green for the advantage
            data = "real"
        else:
            data = "placeholder"
    except FileNotFoundError:
        data = "placeholder"

    if data == "placeholder":
        # Honest placeholder — make it clear these are not the real numbers
        labels = [
            "QUIC\n1-RTT\n(handshake)",
            "QUIC\n0-RTT\n(handshake)",
            "QUIC\n0-RTT\n(first app-data sent)",
        ]
        vals = [93.0, 93.0, 0.25]  # WAN estimate
        errs = [5.0, 5.0, 0.1]
        colors = [QC, QC, "#2ca02c"]

    bars = ax.bar(labels, vals, yerr=errs, capsize=4, color=colors)
    ax.set_ylabel("Latency (ms)")
    title_suffix = (
        "" if data == "real" else " (placeholder — run benchmark for real data)"
    )
    ax.set_title(
        f"0-RTT connection cost: handshake complete + client-side advantage{title_suffix}"
    )

    for b, v, e in zip(bars, vals, errs):
        ax.text(
            b.get_x() + b.get_width() / 2,
            v + max(errs) + 1,
            f"{v:.1f} ms",
            ha="center",
            fontsize=9,
        )

    # Annotate the key insight
    if data == "real":
        ax.text(
            0.5,
            -0.25,
            "0-RTT does NOT reduce server-side handshake time; only client-side CPU time before first send.",
            transform=ax.transAxes,
            ha="center",
            fontsize=8,
            style="italic",
            wrap=True,
        )

    ax.grid(True, axis="y", alpha=0.3)
    fig.tight_layout()
    fig.savefig(f"{OUT}/res_0rtt_dial.png", dpi=150)
    plt.close(fig)


for fn in (fig_scale, fig_multistream, fig_connection, fig_latency, fig_loss, fig_0rtt):
    try:
        fn()
        print("ok:", fn.__name__)
    except Exception as e:
        print("FAIL:", fn.__name__, e)
print("figures in", OUT)
