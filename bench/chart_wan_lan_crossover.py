#!/usr/bin/env python3
"""chart_wan_lan_crossover.py — the QUIC/TCP crossover: LAN vs WAN, large payloads.

Two side-by-side panels (LAN, WAN), each plotting QUIC vs TCP p50 RTT against
payload size. Shows that TCP wins large-on-LAN while QUIC wins large-on-WAN.

Usage: python3 bench/chart_wan_lan_crossover.py <wan_vs_lan_large_msg.csv> -o out.png
"""
import argparse
import csv
import sys


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("csv_file")
    ap.add_argument("-o", "--output", default="bench_charts/compare/wan_lan_crossover.png")
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

    # data[(network, proto)] = list[(size, p50)]
    data = {}
    with open(args.csv_file, newline="") as fh:
        for r in csv.DictReader(fh):
            key = (r["network"], r["proto"].lower())
            data.setdefault(key, []).append((int(r["size"]), float(r["rtt_p50_ms"])))
    for k in data:
        data[k].sort()

    colors = {"quic": "#1f77b4", "tcp": "#ff7f0e"}
    markers = {"quic": "o", "tcp": "s"}

    fig, axes = plt.subplots(1, 2, figsize=(14, 6))
    for ax, net, label in ((axes[0], "lan", "LAN (loopback, ~0 ms RTT)"),
                           (axes[1], "wan", "WAN (EU↔US, ~90 ms RTT)")):
        for proto in ("quic", "tcp"):
            pts = data.get((net, proto))
            if not pts:
                continue
            xs = [p[0] for p in pts]
            ys = [p[1] for p in pts]
            ax.plot(xs, ys, marker=markers[proto], color=colors[proto], linewidth=2,
                    label=proto.upper())
        ax.set_xscale("log", base=2)
        ax.set_yscale("log")
        ax.set_xlabel("Payload size (bytes, log2)", fontsize=11)
        ax.set_ylabel("Relay round-trip p50 (ms, log)", fontsize=11)
        ax.set_title(label, fontsize=12, fontweight="bold")
        ax.grid(True, which="both", ls="--", alpha=0.5)
        ax.legend(fontsize=10)

    fig.suptitle("QUIC vs TCP large-message RTT — the LAN/WAN crossover",
                 fontsize=14, fontweight="bold")
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(args.output, dpi=200)
    print(f"wrote {args.output}")


if __name__ == "__main__":
    main()
