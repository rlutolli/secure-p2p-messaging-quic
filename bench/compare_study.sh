#!/bin/bash
# compare_study.sh — QUIC vs TCP comparative study orchestrator.
#
# Runs the controlled comparison across the project's benchmark dimensions for
# BOTH protocols and writes CSVs in the exact format bench/chart.py consumes,
# plus per-dimension CSVs for the report.
#
# Dimensions:
#   1. Connection  — dial time at scale (dial mode, n sweep)
#   2. Latency     — RTT by payload size (latency mode, size sweep, fixed n)
#   3. Scale       — many peers, low rate (scale mode, n sweep)  [chart.py input]
#   4. Throughput  — message-rate sweep (throughput mode, n=50)
#
# (Multistream / raw-transport HOL-blocking is run separately via the raw_mini
#  tools, since it needs dedicated echo servers.)
#
# Usage:
#   TARGET=63.185.9.233:41748 ./bench/compare_study.sh
set -uo pipefail

TARGET="${TARGET:-}"
ROOM="${ROOM:-benchmark}"
OUTDIR="${OUTDIR:-./bench_results/compare_$(date +%Y%m%d_%H%M%S)}"
PROTOS="${PROTOS:-quic tcp}"

# Sweeps (override via env)
DIAL_PEERS="${DIAL_PEERS:-10 50 100 200 500}"
SCALE_PEERS="${SCALE_PEERS:-10 50 200 500 1000}"
SCALE_DURATION="${SCALE_DURATION:-20}"
SCALE_RATE="${SCALE_RATE:-1}"
SCALE_SIZE="${SCALE_SIZE:-100}"
LAT_SIZES="${LAT_SIZES:-64 256 1024 4096 16384 65536}"
LAT_N="${LAT_N:-10}"
LAT_DURATION="${LAT_DURATION:-5}"
THRU_RATES="${THRU_RATES:-1 5 10 50 100}"
THRU_N="${THRU_N:-50}"
THRU_DURATION="${THRU_DURATION:-10}"
PAUSE="${PAUSE:-8}"

if [[ -z "$TARGET" ]]; then echo "FATAL: set TARGET=host:port"; exit 1; fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "Building loadtest-bin ..."
go build -o loadtest-bin loadtest/main.go || { echo "build failed"; exit 1; }

mkdir -p "$OUTDIR"/{connection,latency,scale,throughput}
echo "Output: $OUTDIR"
echo "Target: $TARGET"

ts() { date '+%H:%M:%S'; }
stagger_for() { local n="$1"; if [[ "$n" -ge 500 ]]; then echo "3ms"; else echo ""; fi; }

run() { # label + args
  local label="$1"; shift
  echo "[$(ts)] $label"
  local st; st="$(stagger_for "${N:-0}")"
  if [[ -n "$st" ]]; then
    ./loadtest-bin "$@" -dial-stagger "$st" 2>>"$OUTDIR/stderr.log"
  else
    ./loadtest-bin "$@" 2>>"$OUTDIR/stderr.log"
  fi
  sleep "$PAUSE"
}

# ── Dimension 1: Connection (dial) ───────────────────────────────────────────
for P in $PROTOS; do
  for N in $DIAL_PEERS; do
    run "connection $P n=$N" -mode dial -proto "$P" -n "$N" -target "$TARGET" \
      -room "$ROOM" -alias-prefix bench -output "$OUTDIR/connection/dial_${P}_${N}.csv"
  done
done

# ── Dimension 2: Latency by payload size ─────────────────────────────────────
for P in $PROTOS; do
  N="$LAT_N"
  run "latency $P sizes=[$LAT_SIZES]" -mode latency -proto "$P" -n "$LAT_N" \
    -sizes "$(echo "$LAT_SIZES" | tr ' ' ',')" -duration "$LAT_DURATION" \
    -target "$TARGET" -room "$ROOM" -alias-prefix bench \
    -output "$OUTDIR/latency/latency_${P}.csv"
done

# ── Dimension 3: Scale sweep (chart.py input) ────────────────────────────────
for P in $PROTOS; do
  ALL="$OUTDIR/scale/loadtest_${P}_all.csv"
  : > "$ALL"; first=1
  for N in $SCALE_PEERS; do
    OUT="$OUTDIR/scale/scale_${P}_${N}.csv"
    run "scale $P n=$N" -mode scale -proto "$P" -n "$N" -rate "$SCALE_RATE" \
      -duration "$SCALE_DURATION" -size "$SCALE_SIZE" -target "$TARGET" \
      -room "$ROOM" -alias-prefix bench -output "$OUT"
    if [[ -f "$OUT" ]]; then
      if [[ $first -eq 1 ]]; then cat "$OUT" >> "$ALL"; first=0; else tail -n +2 "$OUT" >> "$ALL"; fi
    fi
  done
  echo "  → aggregated $ALL"
done

# ── Dimension 4: Throughput rate sweep ───────────────────────────────────────
for P in $PROTOS; do
  N="$THRU_N"
  run "throughput $P rates=[$THRU_RATES]" -mode throughput -proto "$P" -n "$THRU_N" \
    -rates "$(echo "$THRU_RATES" | tr ' ' ',')" -duration "$THRU_DURATION" -size "$SCALE_SIZE" \
    -target "$TARGET" -room "$ROOM" -alias-prefix bench \
    -output "$OUTDIR/throughput/throughput_${P}.csv"
done

echo ""
echo "DONE. Results in $OUTDIR"
