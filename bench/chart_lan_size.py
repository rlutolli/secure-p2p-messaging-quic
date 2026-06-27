#!/usr/bin/env python3
"""chart_lan_size.py — LAN message RTT vs payload size: QUIC vs TCP.

Shows the documented effect that on a clean LAN (near-zero latency) TCP wins on
large payloads because QUIC pays per-packet userspace cost that has no RTT to
hide behind.

Usage:
    python3 bench/chart_lan_size.py <lan_message_rtt.csv> -o <out.png>
"""
import argparse
import csv
import sys


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("csv_file")
    ap.add_argument("-o", "--output", default="bench_charts/compare/lan_latency_by_size.png")
    ap.add_argument("--title", default="LAN (loopback) — Message RTT by Payload Size")
    args = ap.parse_args()

    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError as e:
        print(f"need matplotlib: {e}", file=sys.stderr)
        sys.exit(1)
    try:
        plt.style.use("seaborn-v0_8-whitegrid")
    except Exception:
        pass

    series = {}
    with open(args.csv_file, newline="") as fh:
        for r in csv.DictReader(fh):
            p = r["proto"].lower()
            series.setdefault(p, []).append((int(r["size"]), float(r["rtt_p50_ms"]), float(r["rtt_p95_ms"])))
    for p in series:
        series[p].sort()

    colors = {"quic": "#1f77b4", "tcp": "#ff7f0e"}
    markers = {"quic": "o", "tcp": "s"}

    fig, ax = plt.subplots(figsize=(10, 6))
    for p, pts in series.items():
        sizes = [x[0] for x in pts]
        p50 = [x[1] for x in pts]
        p95 = [x[2] for x in pts]
        ax.plot(sizes, p50, marker=markers.get(p, "D"), color=colors.get(p, "green"),
                linewidth=2, label=f"{p.upper()} p50")
        ax.plot(sizes, p95, marker=markers.get(p, "D"), color=colors.get(p, "green"),
                linewidth=1.5, linestyle="--", alpha=0.7, label=f"{p.upper()} p95")
    ax.set_xscale("log", base=2)
    ax.set_yscale("symlog")
    ax.set_xlabel("Payload size (bytes, log2)", fontsize=12)
    ax.set_ylabel("Relay round-trip time (ms, symlog)", fontsize=12)
    ax.set_title(args.title, fontsize=14, fontweight="bold")
    ax.grid(True, which="both", ls="--", alpha=0.5)
    ax.legend(fontsize=10)
    fig.tight_layout()
    fig.savefig(args.output, dpi=200)
    print(f"wrote {args.output}")


if __name__ == "__main__":
    main()
