#!/bin/bash
# loss_study.sh - focused latency+scale sweeps for the loss/jitter resilience study.
# Run on the CLIENT (US) while tc netem is applied on the relay (EU). Produces the
# same rep_<i>/{latency,scale}/ layout that bench/aggregate_repeats.py consumes, so
# the lossy runs aggregate to mean +/- 95% CI exactly like the clean runs.
#
# Usage (apply netem on the relay first, then):
#   TARGET=63.185.9.233:41748 LABEL=loss1 REPEATS=3 ./bench/loss_study.sh
set -uo pipefail

TARGET="${TARGET:?set TARGET=host:port}"
LABEL="${LABEL:-loss}"
REPEATS="${REPEATS:-3}"
ROOM="${ROOM:-benchmark}"
LAT_SIZES="${LAT_SIZES:-64 1024 16384 65536}"
LAT_N="${LAT_N:-10}"
SCALE_PEERS="${SCALE_PEERS:-50 200 500}"
SCALE_DUR="${SCALE_DUR:-20}"
PROTOS="${PROTOS:-quic tcp}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO"

go build -o loadtest-bin loadtest/main.go || { echo "build failed"; exit 1; }
ROOT="bench_results/repeats_${LABEL}"
mkdir -p "$ROOT"
echo "loss_study LABEL=$LABEL REPEATS=$REPEATS TARGET=$TARGET sizes=[$LAT_SIZES] scale=[$SCALE_PEERS]"

for r in $(seq 1 "$REPEATS"); do
  echo "=== rep $r/$REPEATS ($(date -u +%H:%M:%SZ)) ==="
  for P in $PROTOS; do
    mkdir -p "$ROOT/rep_$r/latency" "$ROOT/rep_$r/scale"
    echo "  latency $P"
    ./loadtest-bin -mode latency -proto "$P" -n "$LAT_N" \
      -sizes "$(echo "$LAT_SIZES" | tr ' ' ',')" -duration 5 \
      -target "$TARGET" -room "$ROOM" -alias-prefix bench \
      -output "$ROOT/rep_$r/latency/latency_$P.csv" 2>>"$ROOT/stderr.log"
    sleep 5
    for N in $SCALE_PEERS; do
      echo "  scale $P n=$N"
      ./loadtest-bin -mode scale -proto "$P" -n "$N" -rate 1 -duration "$SCALE_DUR" -size 100 \
        -target "$TARGET" -room "$ROOM" -alias-prefix bench \
        -output "$ROOT/rep_$r/scale/scale_${P}_${N}.csv" 2>>"$ROOT/stderr.log"
      sleep 5
    done
  done
done
echo "DONE -> $ROOT"
