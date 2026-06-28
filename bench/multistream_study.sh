#!/bin/bash
# multistream_study.sh - repeated multistream sweep: QUIC N streams (1 conn) vs
# TCP N connections. Run on the CLIENT (US). Requires the raw echo servers to be
# running on the target (EU):  bash raw_mini/run_servers.sh
#   -> tcp_raw  on TCP_PORT (9000), quic_diagnostic on QUIC_PORT (9001)
#
# Produces a tidy CSV with one row per (rep, stream-count, proto):
#   proto,streams,size,per_stream_min_ms,per_stream_p50_ms,per_stream_max_ms,
#   per_stream_avg_ms,total_elapsed_ms,rep
# which bench/aggregate_repeats.py-style stats (or a quick pandas/awk) can reduce
# to mean +/- 95% CI per (proto, streams).
#
# Usage:
#   ADDR=63.185.9.233 LABEL=baseline REPEATS=5 ./bench/multistream_study.sh
set -uo pipefail

ADDR="${ADDR:-63.185.9.233}"
TCP_PORT="${TCP_PORT:-9000}"
QUIC_PORT="${QUIC_PORT:-9001}"
STREAMS="${STREAMS:-4 8 16 32}"
SIZE="${SIZE:-1024}"
REPEATS="${REPEATS:-5}"
LABEL="${LABEL:-baseline}"
PAUSE="${PAUSE:-2}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO"

OUT="bench_results/repeats_multistream_${LABEL}"
mkdir -p "$OUT"
CSV="$OUT/multistream.csv"
echo "proto,streams,size,per_stream_min_ms,per_stream_p50_ms,per_stream_max_ms,per_stream_avg_ms,total_elapsed_ms,rep" > "$CSV"

echo "Building multiplex_bench ..."
go build -o /tmp/mbench raw_mini/multiplex_bench.go || { echo "build failed"; exit 1; }

echo "multistream_study: LABEL=$LABEL REPEATS=$REPEATS ADDR=$ADDR streams=[$STREAMS] size=$SIZE"
echo "  (ensure raw_mini/run_servers.sh is running on $ADDR: tcp :$TCP_PORT quic :$QUIC_PORT)"

for r in $(seq 1 "$REPEATS"); do
  echo "=== rep $r/$REPEATS ==="
  for n in $STREAMS; do
    for proto in quic tcp; do
      if [ "$proto" = "quic" ]; then PORTARG="-quicport $QUIC_PORT"; else PORTARG="-tcpport $TCP_PORT"; fi
      row=$(/tmp/mbench -proto "$proto" -n "$n" -size "$SIZE" -addr "$ADDR" $PORTARG -csv 2>/dev/null \
              | grep '^CSV,' | sed 's/^CSV,//')
      if [ -n "$row" ]; then
        echo "${row},${r}" >> "$CSV"
        echo "  rep$r $proto n=$n -> $row"
      else
        echo "  rep$r $proto n=$n -> NO DATA (echo server up on $ADDR:$([ "$proto" = quic ] && echo "$QUIC_PORT" || echo "$TCP_PORT")?)"
      fi
      sleep "$PAUSE"
    done
  done
done

echo "wrote $CSV"
