#!/usr/bin/env python3
"""bench/chart.py — Generate publication-quality benchmark charts from CSV results.

Reads benchmark CSV files (produced by bench/run_all.sh and loadtest/main.go) and
renders a set of comparison PNGs using matplotlib.

Usage:
    python3 bench/chart.py results.csv
    python3 bench/chart.py quic_results.csv tcp_results.csv --compare -o ./bench_charts --title "WAN Benchmark"

Dependencies:
    pip install matplotlib
"""

import argparse
import csv
import os
import sys
from collections import defaultdict


def _check_deps():
    """Ensure matplotlib is available; exit with a helpful message if not."""
    try:
        import matplotlib
        import matplotlib.pyplot as plt
        return plt, matplotlib
    except ImportError as exc:
        print(f"Missing dependency: {exc}", file=sys.stderr)
        print("Please install required packages:", file=sys.stderr)
        print("    pip install matplotlib", file=sys.stderr)
        sys.exit(1)


def _parse_csv(path):
    """Parse a single CSV file into a list of row dicts with numeric conversions."""
    if not os.path.isfile(path):
        print(f"File not found: {path}", file=sys.stderr)
        sys.exit(1)

    numeric_cols = {
        "n", "rate", "duration", "size",
        "dial_ms_avg", "dial_ms_p95",
        "join_ms_avg", "join_ms_p95",
        "total_msgs_sent", "total_msgs_recv",
        "avg_throughput_msgs_per_sec", "avg_throughput_kb_per_sec",
        "min_rtt_ms", "p50_rtt_ms", "p95_rtt_ms", "p99_rtt_ms", "max_rtt_ms",
        "errors_total", "disconnects_total",
    }

    rows = []
    try:
        with open(path, "r", newline="", encoding="utf-8") as fh:
            reader = csv.DictReader(fh)
            if not reader.fieldnames:
                raise ValueError("CSV has no header row")
            for line_no, raw in enumerate(reader, start=2):
                row = {}
                for k, v in raw.items():
                    if v is None:
                        continue
                    v = v.strip()
                    if not v:
                        row[k] = None
                        continue
                    if k in numeric_cols:
                        try:
                            if "." in v:
                                row[k] = float(v)
                            else:
                                row[k] = int(v)
                        except ValueError:
                            print(
                                f"{path}:{line_no}: cannot convert '{v}' to number for column '{k}'",
                                file=sys.stderr,
                            )
                            row[k] = None
                    else:
                        row[k] = v
                rows.append(row)
    except Exception as exc:
        print(f"Failed to parse {path}: {exc}", file=sys.stderr)
        sys.exit(1)

    return rows


def _collect(rows):
    """Group rows by (protocol, n) and sort by n for each protocol."""
    proto_groups = defaultdict(list)
    for r in rows:
        proto = r.get("proto") or "unknown"
        proto_groups[proto].append(r)
    for proto in proto_groups:
        proto_groups[proto].sort(key=lambda r: (r.get("n") or 0))
    return dict(proto_groups)


def _protocol_color(protocol):
    """Return a consistent color for known protocols."""
    p = (protocol or "").lower()
    if p == "quic":
        return "#1f77b4"  # matplotlib default blue
    if p == "tcp":
        return "#ff7f0e"  # matplotlib default orange
    return "#2ca02c"  # green fallback


def _protocol_marker(protocol):
    p = (protocol or "").lower()
    if p == "quic":
        return "o"
    if p == "tcp":
        return "s"
    return "D"


def plot_latency_vs_scale(plt, groups, output_dir, title_prefix=""):
    """Latency vs Scale (p50, p95, p99)."""
    fig, ax = plt.subplots(figsize=(10, 6))
    ax.set_yscale("log")

    for proto, rows in groups.items():
        ns = [r["n"] for r in rows]
        p50 = [r.get("p50_rtt_ms") for r in rows]
        p95 = [r.get("p95_rtt_ms") for r in rows]
        p99 = [r.get("p99_rtt_ms") for r in rows]
        min_rtt = [r.get("min_rtt_ms") for r in rows]
        max_rtt = [r.get("max_rtt_ms") for r in rows]
        color = _protocol_color(proto)
        marker = _protocol_marker(proto)

        if all(v is not None for v in p50):
            ax.plot(ns, p50, marker=marker, color=color, linestyle="-",
                    label=f"{proto} p50", linewidth=2)
        if all(v is not None for v in p95):
            ax.plot(ns, p95, marker=marker, color=color, linestyle="--",
                    label=f"{proto} p95", linewidth=2)
        if all(v is not None for v in p99):
            ax.plot(ns, p99, marker=marker, color=color, linestyle=":",
                    label=f"{proto} p99", linewidth=2)

        # Shading between min and max if available
        if all(v is not None for v in min_rtt) and all(v is not None for v in max_rtt):
            ax.fill_between(ns, min_rtt, max_rtt, color=color, alpha=0.1)

    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Round-trip time (ms, log scale)", fontsize=12)
    title = "Latency vs Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax.set_title(title, fontsize=14, fontweight="bold")
    ax.legend(fontsize=9, loc="upper left")
    ax.grid(True, which="both", ls="--", alpha=0.5)

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "latency_vs_scale.png"), dpi=300)
    plt.close(fig)


def plot_throughput_vs_scale(plt, groups, output_dir, title_prefix=""):
    """Throughput vs Scale with optional secondary axis for KB/s."""
    fig, ax1 = plt.subplots(figsize=(10, 6))

    for proto, rows in groups.items():
        ns = [r["n"] for r in rows]
        msgs = [r.get("avg_throughput_msgs_per_sec") for r in rows]
        kbs = [r.get("avg_throughput_kb_per_sec") for r in rows]
        color = _protocol_color(proto)
        marker = _protocol_marker(proto)

        if all(v is not None for v in msgs):
            ax1.plot(ns, msgs, marker=marker, color=color, linestyle="-",
                     label=f"{proto} msgs/s", linewidth=2)

    ax1.set_xlabel("Number of peers (n)", fontsize=12)
    ax1.set_ylabel("Throughput (messages / sec)", fontsize=12, color="black")
    ax1.tick_params(axis="y", labelcolor="black")
    ax1.grid(True, ls="--", alpha=0.5)

    # Secondary axis only if at least one group has KB/s data
    has_kb = any(
        all(r.get("avg_throughput_kb_per_sec") is not None for r in rows)
        for rows in groups.values()
    )
    if has_kb:
        ax2 = ax1.twinx()
        for proto, rows in groups.items():
            ns = [r["n"] for r in rows]
            kbs = [r.get("avg_throughput_kb_per_sec") for r in rows]
            color = _protocol_color(proto)
            marker = _protocol_marker(proto)
            if all(v is not None for v in kbs):
                ax2.plot(ns, kbs, marker=marker, color=color, linestyle="--",
                         label=f"{proto} KB/s", linewidth=2)
        ax2.set_ylabel("Throughput (KB / sec)", fontsize=12, color="gray")
        ax2.tick_params(axis="y", labelcolor="gray")
        # Combine legends
        lines1, labels1 = ax1.get_legend_handles_labels()
        lines2, labels2 = ax2.get_legend_handles_labels()
        ax1.legend(lines1 + lines2, labels1 + labels2, fontsize=9, loc="upper left")
    else:
        ax1.legend(fontsize=9, loc="upper left")

    title = "Throughput vs Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax1.set_title(title, fontsize=14, fontweight="bold")

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "throughput_vs_scale.png"), dpi=300)
    plt.close(fig)


def plot_dial_vs_scale(plt, groups, output_dir, title_prefix=""):
    """Dial time vs Scale (avg and p95)."""
    fig, ax = plt.subplots(figsize=(10, 6))

    protocols = list(groups.keys())
    # Determine all unique n values across groups
    all_ns = sorted({r["n"] for rows in groups.values() for r in rows})
    if not all_ns:
        plt.close(fig)
        return

    width = 0.35
    n_protocols = len(protocols)
    x = range(len(all_ns))

    for idx, proto in enumerate(protocols):
        rows = groups[proto]
        row_map = {r["n"]: r for r in rows}
        avg_vals = [row_map.get(n, {}).get("dial_ms_avg") for n in all_ns]
        p95_vals = [row_map.get(n, {}).get("dial_ms_p95") for n in all_ns]
        color = _protocol_color(proto)
        offset = width * (idx - (n_protocols - 1) / 2)

        # Plot avg as bars, p95 as line with same color
        valid_avg = [(i, v) for i, v in enumerate(avg_vals) if v is not None]
        if valid_avg:
            xs, ys = zip(*valid_avg)
            ax.bar([xi + offset for xi in xs], ys, width=width, color=color,
                   alpha=0.7, label=f"{proto} avg", zorder=2)
        valid_p95 = [(i, v) for i, v in enumerate(p95_vals) if v is not None]
        if valid_p95:
            xs, ys = zip(*valid_p95)
            ax.plot([xi + offset for xi in xs], ys, marker="^", color=color,
                    linestyle="--", label=f"{proto} p95", linewidth=2, zorder=3)

    ax.set_xticks(x)
    ax.set_xticklabels(all_ns)
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Dial time (ms)", fontsize=12)
    title = "Dial Time vs Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax.set_title(title, fontsize=14, fontweight="bold")
    ax.legend(fontsize=9, loc="upper left")
    ax.grid(True, axis="y", ls="--", alpha=0.5)

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "dial_vs_scale.png"), dpi=300)
    plt.close(fig)


def plot_join_vs_scale(plt, groups, output_dir, title_prefix=""):
    """Join time vs Scale (avg and p95)."""
    fig, ax = plt.subplots(figsize=(10, 6))

    protocols = list(groups.keys())
    all_ns = sorted({r["n"] for rows in groups.values() for r in rows})
    if not all_ns:
        plt.close(fig)
        return

    width = 0.35
    n_protocols = len(protocols)
    x = range(len(all_ns))

    for idx, proto in enumerate(protocols):
        rows = groups[proto]
        row_map = {r["n"]: r for r in rows}
        avg_vals = [row_map.get(n, {}).get("join_ms_avg") for n in all_ns]
        p95_vals = [row_map.get(n, {}).get("join_ms_p95") for n in all_ns]
        color = _protocol_color(proto)
        offset = width * (idx - (n_protocols - 1) / 2)

        valid_avg = [(i, v) for i, v in enumerate(avg_vals) if v is not None]
        if valid_avg:
            xs, ys = zip(*valid_avg)
            ax.bar([xi + offset for xi in xs], ys, width=width, color=color,
                   alpha=0.7, label=f"{proto} avg", zorder=2)
        valid_p95 = [(i, v) for i, v in enumerate(p95_vals) if v is not None]
        if valid_p95:
            xs, ys = zip(*valid_p95)
            ax.plot([xi + offset for xi in xs], ys, marker="^", color=color,
                    linestyle="--", label=f"{proto} p95", linewidth=2, zorder=3)

    ax.set_xticks(x)
    ax.set_xticklabels(all_ns)
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Join time (ms)", fontsize=12)
    title = "Join Time vs Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax.set_title(title, fontsize=14, fontweight="bold")
    ax.legend(fontsize=9, loc="upper left")
    ax.grid(True, axis="y", ls="--", alpha=0.5)

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "join_vs_scale.png"), dpi=300)
    plt.close(fig)


def plot_errors_vs_scale(plt, groups, output_dir, title_prefix=""):
    """Errors & disconnects vs Scale as stacked bars."""
    fig, ax = plt.subplots(figsize=(10, 6))

    protocols = list(groups.keys())
    all_ns = sorted({r["n"] for rows in groups.values() for r in rows})
    if not all_ns:
        plt.close(fig)
        return

    width = 0.35
    n_protocols = len(protocols)
    x = range(len(all_ns))

    for idx, proto in enumerate(protocols):
        rows = groups[proto]
        row_map = {r["n"]: r for r in rows}
        errors = [row_map.get(n, {}).get("errors_total") for n in all_ns]
        discos = [row_map.get(n, {}).get("disconnects_total") for n in all_ns]
        color = _protocol_color(proto)
        offset = width * (idx - (n_protocols - 1) / 2)

        valid_err = [(i, v if v is not None else 0) for i, v in enumerate(errors)]
        valid_dis = [(i, v if v is not None else 0) for i, v in enumerate(discos)]

        if valid_err and valid_dis:
            xs_err, ys_err = zip(*valid_err)
            xs_dis, ys_dis = zip(*valid_dis)
            # Stacked bars
            ax.bar([xi + offset for xi in xs_err], ys_err, width=width,
                   color=color, alpha=0.9, label=f"{proto} errors", zorder=2)
            ax.bar([xi + offset for xi in xs_dis], ys_dis, width=width,
                   color=color, alpha=0.4, bottom=ys_err,
                   label=f"{proto} disconnects", zorder=2)

    ax.set_xticks(x)
    ax.set_xticklabels(all_ns)
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Count", fontsize=12)
    title = "Errors & Disconnects vs Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax.set_title(title, fontsize=14, fontweight="bold")
    ax.legend(fontsize=9, loc="upper left")
    ax.grid(True, axis="y", ls="--", alpha=0.5)

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "errors_vs_scale.png"), dpi=300)
    plt.close(fig)


def plot_rtt_distribution(plt, groups, output_dir, title_prefix=""):
    """RTT Distribution: custom box-and-whisker per (protocol, n) using summary stats."""
    fig, ax = plt.subplots(figsize=(10, 6))

    protocols = list(groups.keys())
    all_ns = sorted({r["n"] for rows in groups.values() for r in rows})
    if not all_ns or len(all_ns) < 2:
        plt.close(fig)
        return

    width = 0.35
    n_protocols = len(protocols)

    for p_idx, proto in enumerate(protocols):
        rows = groups[proto]
        row_map = {r["n"]: r for r in rows}
        color = _protocol_color(proto)
        offset = width * (p_idx - (n_protocols - 1) / 2)

        for n_idx, n in enumerate(all_ns):
            r = row_map.get(n)
            if not r:
                continue
            vals = [r.get(k) for k in ("min_rtt_ms", "p50_rtt_ms", "p95_rtt_ms", "p99_rtt_ms", "max_rtt_ms")]
            if any(v is None for v in vals):
                continue
            min_v, p50, p95, p99, max_v = vals
            x_center = n_idx + offset

            # Box from p50 to p95, whiskers to min and max, p99 as a notch
            box_w = width * 0.8
            rect = plt.matplotlib.patches.Rectangle(
                (x_center - box_w / 2, min(p50, p95)), box_w, abs(p95 - p50),
                facecolor=color, edgecolor="black", alpha=0.7, zorder=3,
            )
            ax.add_patch(rect)
            # Median line
            ax.plot([x_center - box_w / 2, x_center + box_w / 2], [p50, p50],
                    color="white", linewidth=2, zorder=4)
            # Whiskers
            ax.plot([x_center, x_center], [min_v, min(p50, p95)], color="black", linewidth=1, zorder=3)
            ax.plot([x_center, x_center], [max(p50, p95), max_v], color="black", linewidth=1, zorder=3)
            # Caps
            ax.plot([x_center - box_w / 4, x_center + box_w / 4], [min_v, min_v], color="black", linewidth=1, zorder=3)
            ax.plot([x_center - box_w / 4, x_center + box_w / 4], [max_v, max_v], color="black", linewidth=1, zorder=3)
            # P99 notch
            ax.plot([x_center], [p99], marker="D", color="red", markersize=4, zorder=4)

    ax.set_xticks(range(len(all_ns)))
    ax.set_xticklabels(all_ns)
    ax.set_xlabel("Number of peers (n)", fontsize=12)
    ax.set_ylabel("Round-trip time (ms)", fontsize=12)
    title = "RTT Distribution by Scale"
    if title_prefix:
        title = f"{title_prefix} — {title}"
    ax.set_title(title, fontsize=14, fontweight="bold")

    # Build a tiny legend manually
    from matplotlib.lines import Line2D
    from matplotlib.patches import Patch
    legend_elements = []
    for proto in protocols:
        legend_elements.append(Patch(facecolor=_protocol_color(proto), edgecolor="black",
                                     label=f"{proto} p50–p95 box"))
    legend_elements.append(Line2D([0], [0], marker="D", color="w", markerfacecolor="red",
                                   markersize=6, label="p99"))
    ax.legend(handles=legend_elements, fontsize=9, loc="upper left")
    ax.grid(True, axis="y", ls="--", alpha=0.5)

    fig.tight_layout()
    fig.savefig(os.path.join(output_dir, "rtt_distribution.png"), dpi=300)
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(
        description="Generate benchmark charts from CSV results.",
    )
    parser.add_argument(
        "csv_files",
        nargs="+",
        help="One or more CSV benchmark result files",
    )
    parser.add_argument(
        "-o", "--output-dir",
        default="./bench_charts",
        help="Directory to save PNG charts (default: ./bench_charts)",
    )
    parser.add_argument(
        "--compare",
        action="store_true",
        help="Compare multiple CSV files on the same charts",
    )
    parser.add_argument(
        "--title",
        default="",
        help="Optional chart title prefix",
    )
    args = parser.parse_args()

    plt, matplotlib = _check_deps()

    # Apply a clean style
    available_styles = plt.style.available
    preferred = ["seaborn-v0_8-whitegrid", "seaborn-whitegrid", "ggplot", "bmh"]
    chosen = None
    for s in preferred:
        if s in available_styles:
            chosen = s
            break
    if chosen:
        plt.style.use(chosen)
    else:
        plt.style.use("default")

    # Read all CSVs
    all_rows = []
    for path in args.csv_files:
        all_rows.extend(_parse_csv(path))

    if not all_rows:
        print("No data rows found in provided CSV files.", file=sys.stderr)
        sys.exit(1)

    os.makedirs(args.output_dir, exist_ok=True)

    # If --compare is set or multiple CSVs were given, merge everything.
    # Even with a single file we still group by protocol so the same code path works.
    groups = _collect(all_rows)

    plot_latency_vs_scale(plt, groups, args.output_dir, args.title)
    plot_throughput_vs_scale(plt, groups, args.output_dir, args.title)
    plot_dial_vs_scale(plt, groups, args.output_dir, args.title)
    plot_join_vs_scale(plt, groups, args.output_dir, args.title)
    plot_errors_vs_scale(plt, groups, args.output_dir, args.title)

    # RTT distribution only when comparing or if we have enough data points
    if args.compare or len(args.csv_files) > 1 or len(all_rows) >= 3:
        plot_rtt_distribution(plt, groups, args.output_dir, args.title)

    print(f"Charts saved to: {os.path.abspath(args.output_dir)}")
    sys.exit(0)


if __name__ == "__main__":
    main()
