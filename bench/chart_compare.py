#!/usr/bin/env python3
"""bench/chart_compare.py — QUIC vs TCP comparative study charts.

Generates side-by-side QUIC vs TCP charts for each benchmark dimension of the
comparative study produced by bench/compare_study.sh:

  1. Connection — dial time vs number of peers (from connection/dial_<proto>_<n>.csv)
  2. Latency    — message RTT vs payload size (from latency/latency_<proto>.csv)
  3. Scale      — RTT and delivery vs peers   (from scale/loadtest_<proto>_all.csv)
  4. Throughput — sustained msg/s vs offered rate (from throughput/throughput_<proto>.csv)

Usage:
    python3 bench/chart_compare.py <run_dir> [-o OUTDIR] [--title PREFIX]

Dependencies: matplotlib
"""
import argparse
import csv
import glob
import os
import re
import sys


def _mpl():
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
        return plt
    except ImportError as exc:
        print(f"Missing dependency: {exc}\n  pip install matplotlib", file=sys.stderr)
        sys.exit(1)


QUIC_C = "#1f77b4"
TCP_C = "#ff7f0e"


def _color(proto):
    return QUIC_C if proto.lower() == "quic" else TCP_C


def _marker(proto):
    return "o" if proto.lower() == "quic" else "s"


def _read(path):
    with open(path, newline="", encoding="utf-8") as fh:
        return list(csv.DictReader(fh))


def _f(row, key):
    v = row.get(key)
    if v is None or v.strip() == "":
        return None
    try:
        return float(v)
    except ValueError:
        return None


# ── 1. Connection: dial time vs peers ────────────────────────────────────────
def chart_connection(plt, run_dir, outdir, title):
    files = glob.glob(os.path.join(run_dir, "connection", "dial_*.csv"))
    if not files:
        return
    # data[proto] = list of (n, avg, p95)
    data = {}
    for path in files:
        m = re.search(r"dial_(quic|tcp)_(\d+)\.csv$", os.path.basename(path))
        if not m:
            continue
        proto, n = m.group(1), int(m.group(2))
        rows = _read(path)
        if not rows:
            continue
        r = rows[0]
        data.setdefault(proto, []).append((n, _f(r, "dial_ms_avg"), _f(r, "dial_ms_p95")))
    if not data:
        return
    fig, ax = plt.subplots(figsize=(10, 6))
    for proto in sorted(data):
        pts = sorted(data[proto])
        ns = [p[0] for p in pts]
        avg = [p[1] for p in pts]
        p95 = [p[2] for p in pts]
        ax.plot(ns, avg, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()} avg")
        ax.plot(ns, p95, marker=_marker(proto), color=_color(proto), linewidth=1.5,
                linestyle="--", alpha=0.7, label=f"{proto.upper()} p95")
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Dial / connection setup time (ms)", fontsize=12)
    ax.set_title(f"{title} — Connection Setup: QUIC vs TCP", fontsize=14, fontweight="bold")
    ax.grid(True, ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    ax.set_ylim(bottom=0)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_connection.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")


# ── 2. Latency: RTT vs payload size ──────────────────────────────────────────
def chart_latency(plt, run_dir, outdir, title):
    files = {p: os.path.join(run_dir, "latency", f"latency_{p}.csv") for p in ("quic", "tcp")}
    series = {}
    for proto, path in files.items():
        if not os.path.isfile(path):
            continue
        rows = _read(path)
        pts = []
        for r in rows:
            size = _f(r, "size")
            p50 = _f(r, "rtt_p50")
            p95 = _f(r, "rtt_p95")
            if size is not None and p50 is not None:
                pts.append((size, p50, p95))
        if pts:
            series[proto] = sorted(pts)
    if not series:
        return
    fig, ax = plt.subplots(figsize=(10, 6))
    for proto, pts in series.items():
        sizes = [p[0] for p in pts]
        p50 = [p[1] for p in pts]
        p95 = [p[2] for p in pts]
        ax.plot(sizes, p50, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()} p50")
        ax.plot(sizes, p95, marker=_marker(proto), color=_color(proto), linewidth=1.5,
                linestyle="--", alpha=0.7, label=f"{proto.upper()} p95")
    ax.set_xscale("log", base=2)
    ax.set_xlabel("Payload size (bytes, log2)", fontsize=12)
    ax.set_ylabel("Message round-trip time (ms)", fontsize=12)
    ax.set_title(f"{title} — Latency by Payload Size: QUIC vs TCP", fontsize=14, fontweight="bold")
    ax.grid(True, which="both", ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_latency.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")


# ── 3. Scale: RTT + delivery vs peers ────────────────────────────────────────
def chart_scale(plt, run_dir, outdir, title):
    files = {p: os.path.join(run_dir, "scale", f"loadtest_{p}_all.csv") for p in ("quic", "tcp")}
    series = {}
    for proto, path in files.items():
        if not os.path.isfile(path):
            continue
        rows = _read(path)
        pts = []
        for r in rows:
            n = _f(r, "n")
            if n is None:
                continue
            sent = _f(r, "total_msgs_sent") or 0
            recv = _f(r, "total_msgs_recv") or 0
            deliv = (recv / sent * 100.0) if sent > 0 else 0.0
            pts.append((n, _f(r, "p50_rtt_ms"), _f(r, "p95_rtt_ms"), deliv))
        if pts:
            series[proto] = sorted(pts)
    if not series:
        return
    # RTT vs peers (log y)
    fig, ax = plt.subplots(figsize=(10, 6))
    ax.set_yscale("log")
    for proto, pts in series.items():
        ns = [p[0] for p in pts]
        p50 = [p[1] for p in pts]
        p95 = [p[2] for p in pts]
        ax.plot(ns, p50, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()} p50")
        ax.plot(ns, p95, marker=_marker(proto), color=_color(proto), linewidth=1.5,
                linestyle="--", alpha=0.7, label=f"{proto.upper()} p95")
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Broadcast RTT (ms, log scale)", fontsize=12)
    ax.set_title(f"{title} — Scale: Broadcast RTT vs Peers", fontsize=14, fontweight="bold")
    ax.grid(True, which="both", ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_scale_rtt.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")

    # Delivery ratio vs peers
    fig, ax = plt.subplots(figsize=(10, 6))
    for proto, pts in series.items():
        ns = [p[0] for p in pts]
        deliv = [p[3] for p in pts]
        ax.plot(ns, deliv, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()}")
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Delivery ratio (%)", fontsize=12)
    ax.set_ylim(0, 105)
    ax.axhline(100, color="green", ls=":", alpha=0.5)
    ax.set_title(f"{title} — Scale: Message Delivery vs Peers", fontsize=14, fontweight="bold")
    ax.grid(True, ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_scale_delivery.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")


# ── 4. Throughput: sustained msg/s vs offered rate ───────────────────────────
def chart_throughput(plt, run_dir, outdir, title):
    files = {p: os.path.join(run_dir, "throughput", f"throughput_{p}.csv") for p in ("quic", "tcp")}
    series = {}
    for proto, path in files.items():
        if not os.path.isfile(path):
            continue
        rows = _read(path)
        pts = []
        for r in rows:
            rate = _f(r, "rate")
            sent = _f(r, "msgs_sent") or 0
            recv = _f(r, "msgs_recv") or 0
            sustained = _f(r, "avg_throughput_msgs_per_sec")
            deliv = (recv / sent * 100.0) if sent > 0 else 0.0
            if rate is not None:
                pts.append((rate, sustained, deliv))
        if pts:
            series[proto] = sorted(pts)
    if not series:
        return
    fig, ax = plt.subplots(figsize=(10, 6))
    for proto, pts in series.items():
        rates = [p[0] for p in pts]
        sustained = [p[1] for p in pts]
        ax.plot(rates, sustained, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()} sustained")
    # ideal line
    all_rates = sorted({p[0] for pts in series.values() for p in pts})
    ax.plot(all_rates, all_rates, color="gray", ls=":", alpha=0.6, label="ideal (offered=sustained)")
    ax.set_xlabel("Offered rate (msg/s/peer)", fontsize=12)
    ax.set_ylabel("Sustained throughput (msg/s/peer)", fontsize=12)
    ax.set_title(f"{title} — Throughput: QUIC vs TCP (n=50)", fontsize=14, fontweight="bold")
    ax.grid(True, ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_throughput.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")


# ── 5. Multistream: parallel streams (HOL blocking) ──────────────────────────
def chart_multistream(plt, run_dir, outdir, title):
    path = os.path.join(run_dir, "multistream", "multistream.csv")
    if not os.path.isfile(path):
        return
    rows = _read(path)
    series = {}
    for r in rows:
        proto = (r.get("proto") or "").lower()
        streams = _f(r, "streams")
        total = _f(r, "total_elapsed_ms")
        if proto and streams is not None and total is not None:
            series.setdefault(proto, []).append((streams, total))
    if not series:
        return
    fig, ax = plt.subplots(figsize=(10, 6))
    for proto in sorted(series):
        pts = sorted(series[proto])
        xs = [p[0] for p in pts]
        ys = [p[1] for p in pts]
        ax.plot(xs, ys, marker=_marker(proto), color=_color(proto), linewidth=2,
                label=f"{proto.upper()} total wall-clock")
    ax.set_xlabel("Parallel streams / connections (n)", fontsize=12)
    ax.set_ylabel("Total time to complete all streams (ms)", fontsize=12)
    ax.set_title(f"{title} — Multistream: QUIC streams vs TCP connections",
                 fontsize=14, fontweight="bold")
    ax.grid(True, ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    ax.set_ylim(bottom=0)
    fig.tight_layout()
    out = os.path.join(outdir, "compare_multistream.png")
    fig.savefig(out, dpi=200)
    plt.close(fig)
    print(f"  wrote {out}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_dir", help="comparative study output directory")
    ap.add_argument("-o", "--output-dir", default=None)
    ap.add_argument("--title", default="WAN QUIC vs TCP")
    args = ap.parse_args()

    outdir = args.output_dir or os.path.join(args.run_dir, "charts")
    os.makedirs(outdir, exist_ok=True)
    plt = _mpl()
    try:
        plt.style.use("seaborn-v0_8-whitegrid")
    except Exception:
        pass

    print(f"Charts → {outdir}")
    chart_connection(plt, args.run_dir, outdir, args.title)
    chart_latency(plt, args.run_dir, outdir, args.title)
    chart_scale(plt, args.run_dir, outdir, args.title)
    chart_throughput(plt, args.run_dir, outdir, args.title)
    chart_multistream(plt, args.run_dir, outdir, args.title)
    print("Done.")


if __name__ == "__main__":
    main()
