#!/bin/bash
# bench/run_full.sh — comprehensive benchmark orchestrator for secure-p2p-messaging-quic
#
# Runs all 5 benchmark dimensions for both TCP and QUIC:
#   1. Connection (dial time at scale)
#   2. Message Latency (RTT by size)
#   3. Multi-Stream (raw transport)
#   4. Scale (many peers)
#   5. Throughput (msg rate sweep)
#
# Usage:
#   ./bench/run_full.sh --target 35.157.17.0:55852 --room benchmark [options]
#
# Options (override environment variables):
#   --target ADDR:PORT      Relay address (env: TARGET, required)
#   --room NAME             Room name (env: ROOM, default: benchmark)
#   --duration SEC          Duration per test (env: DURATION, default: 30)
#   --output-dir DIR        Output directory (env: OUTPUT_DIR,
#                           default: ./bench_results/run_$(date +%Y%m%d_%H%M%S))
#   --skip-raw              Skip raw transport benchmarks (env: SKIP_RAW=1)
#   --help, -h              Show this help
#
# Examples:
#   TARGET=35.157.17.0:55852 ./bench/run_full.sh
#   ./bench/run_full.sh --target 35.157.17.0:55852 --duration 60 --skip-raw
set -euo pipefail

# ─── Helpers ─────────────────────────────────────────────────────────────────
sep()  { printf '=%.0s' {1..72}; printf '\n'; }
hdr()  { sep; printf "  %s\n" "$*"; sep; }
ts()   { date '+%Y-%m-%d %H:%M:%S'; }

show_help() {
cat <<EOF
bench/run_full.sh — comprehensive benchmark orchestrator

Runs all 5 benchmark dimensions (connection, latency, multistream, scale,
throughput) for both TCP and QUIC protocols.

Usage:
  ./bench/run_full.sh --target 35.157.17.0:55852 --room benchmark [options]

Options (override environment variables):
  --target ADDR:PORT      Relay address (env: TARGET, required)
  --room NAME             Room name (env: ROOM, default: benchmark)
  --duration SEC          Duration per test (env: DURATION, default: 30)
  --output-dir DIR        Output directory (env: OUTPUT_DIR,
                          default: ./bench_results/run_\$(date +%Y%m%d_%H%M%S))
  --skip-raw              Skip raw transport benchmarks (env: SKIP_RAW=1)
  --help, -h              Show this help

Examples:
  TARGET=35.157.17.0:55852 ./bench/run_full.sh
  ./bench/run_full.sh --target 35.157.17.0:55852 --duration 60 --skip-raw
EOF
}

# ─── Defaults ──────────────────────────────────────────────────────────────────
TARGET="${TARGET:-}"
ROOM="${ROOM:-benchmark}"
DURATION="${DURATION:-30}"
OUTPUT_DIR="${OUTPUT_DIR:-}"
SKIP_RAW="${SKIP_RAW:-0}"

# Scale parameters (override with env vars to dial up/down)
DIAL_PEERS="${DIAL_PEERS:-10 50 100 200 500}"
SCALE_PEERS="${SCALE_PEERS:-50 200 500 1000}"
THROUGHPUT_RATES="${THROUGHPUT_RATES:-1 5 10 50 100}"
STREAM_COUNTS="${STREAM_COUNTS:-8}"

# ─── CLI arg parsing ───────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)       TARGET="$2"; shift 2 ;;
    --room)         ROOM="$2";   shift 2 ;;
    --duration)     DURATION="$2"; shift 2 ;;
    --output-dir)   OUTPUT_DIR="$2"; shift 2 ;;
    --skip-raw)     SKIP_RAW=1; shift ;;
    --help|-h)      show_help; exit 0 ;;
    --)             shift; break ;;
    *) echo "Unknown option: $1"; show_help; exit 1 ;;
  esac
done

if [[ -z "$OUTPUT_DIR" ]]; then
  OUTPUT_DIR="./bench_results/run_$(date +%Y%m%d_%H%M%S)"
fi

# ─── Resolve repo root ─────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# ─── Pre-flight checks ───────────────────────────────────────────────────────
hdr "Pre-flight checks  $(ts)"

if [[ -z "${TARGET}" ]]; then
  echo "FATAL: --target or TARGET env var is required"
  show_help
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "FATAL: 'go' command not found in PATH"
  exit 1
fi
echo "  → $(go version)"

if [[ ! -f "loadtest/main.go" ]]; then
  echo "FATAL: loadtest/main.go not found in repository root"
  exit 1
fi

echo "  Building loadtest binary ..."
if ! go build -o loadtest-bin loadtest/main.go; then
  echo "FATAL: failed to build loadtest binary"
  exit 1
fi
echo "  → loadtest built successfully"

HOST="${TARGET%%:*}"
PORT="${TARGET##*:}"

echo "  Checking reachability of $TARGET ..."
REACHABLE=false
if command -v nc >/dev/null 2>&1; then
  if nc -z -w5 "$HOST" "$PORT" 2>/dev/null; then
    REACHABLE=true
    echo "  ✓ TCP connect to $HOST:$PORT succeeded"
  fi
elif timeout 5 bash -c "echo >/dev/tcp/$HOST/$PORT" 2>/dev/null; then
  REACHABLE=true
  echo "  ✓ TCP connect to $HOST:$PORT succeeded"
fi

if [[ "$REACHABLE" == "false" ]]; then
  if ping -c 1 -W 3 "$HOST" >/dev/null 2>&1; then
    echo "  ✓ Host $HOST responds to ICMP ping (port $PORT not tested)"
  else
    echo "  ⚠ WARNING: $TARGET appears unreachable. Continuing anyway ..."
  fi
fi

mkdir -p "$OUTPUT_DIR"/{connection,latency,multistream,scale,throughput}
echo "  → OUTPUT_DIR: $(cd "$OUTPUT_DIR" && pwd)"

# ─── Run tracking for sleep logic ──────────────────────────────────────────────
RUN_COUNT=0
# Calculate total runs dynamically
dial_count=$(echo "$DIAL_PEERS" | wc -w)
scale_count=$(echo "$SCALE_PEERS" | wc -w)
throughput_count=$(echo "$THROUGHPUT_RATES" | wc -w)
latency_count=6  # 6 sizes
TOTAL_RUNS=$(( (dial_count + scale_count + throughput_count + latency_count) * 2 ))
if [[ "$SKIP_RAW" != "1" && "$SKIP_RAW" != "true" ]]; then
  TOTAL_RUNS=$((TOTAL_RUNS + 2))
fi

maybe_sleep() {
  RUN_COUNT=$((RUN_COUNT + 1))
  if [[ "$RUN_COUNT" -lt "$TOTAL_RUNS" ]]; then
    echo "  Sleeping 5s to let the relay recover ..."
    sleep 5
  fi
}

# ─── Dimension 1: Connection (dial time at scale) ────────────────────────────
hdr "Dimension 1: Connection (dial time at scale)  $(ts)"

for PROTO in tcp quic; do
  for N in $DIAL_PEERS; do
    OUT_CSV="$OUTPUT_DIR/connection/dial_${PROTO}_${N}.csv"
    echo "  [$(ts)] dial | proto=$PROTO | n=$N"
    ./loadtest-bin \
      -mode dial \
      -proto "$PROTO" \
      -n "$N" \
      -target "$TARGET" \
      -output "$OUT_CSV" || echo "  [WARN] dial $PROTO n=$N failed"
    maybe_sleep
  done
done

# ─── Dimension 2: Message Latency (RTT by size) ──────────────────────────────
hdr "Dimension 2: Message Latency (RTT by size)  $(ts)"

for PROTO in tcp quic; do
  for SIZE in 64 256 1024 4096 16384 65536; do
    OUT_CSV="$OUTPUT_DIR/latency/latency_${PROTO}_${SIZE}.csv"
    echo "  [$(ts)] latency | proto=$PROTO | n=10 | size=$SIZE"
    ./loadtest-bin \
      -mode latency \
      -proto "$PROTO" \
      -n 10 \
      -sizes "$SIZE" \
      -duration 5 \
      -target "$TARGET" \
      -room "$ROOM" \
      -output "$OUT_CSV" || echo "  [WARN] latency $PROTO size=$SIZE failed"
    maybe_sleep
  done
done

# ─── Dimension 3: Multi-Stream (raw transport) ───────────────────────────────
if [[ "$SKIP_RAW" != "1" && "$SKIP_RAW" != "true" ]]; then
  hdr "Dimension 3: Multi-Stream (raw transport)  $(ts)"

  for PROTO in tcp quic; do
    OUT_LOG="$OUTPUT_DIR/multistream/mplex_${PROTO}.log"
    echo "  [$(ts)] multistream | proto=$PROTO | n=$STREAM_COUNTS | addr=$HOST | port=$PORT"
    if go run raw_mini/multiplex_bench.go \
      -proto "$PROTO" \
      -n "$STREAM_COUNTS" \
      -addr "$HOST" \
      -tcpport "$PORT" \
      -quicport "$PORT" > "$OUT_LOG" 2>&1; then
      echo "  ✓ multistream $PROTO completed"
    else
      echo "  ✗ multistream $PROTO failed — see $OUT_LOG"
    fi
    maybe_sleep
  done
else
  echo "  [INFO] Raw benchmarks skipped (--skip-raw or SKIP_RAW set)"
fi

# ─── Dimension 4: Scale (many peers) ───────────────────────────────────────────
hdr "Dimension 4: Scale (many peers)  $(ts)"

for PROTO in tcp quic; do
  for N in $SCALE_PEERS; do
    OUT_CSV="$OUTPUT_DIR/scale/scale_${PROTO}_${N}.csv"
    echo "  [$(ts)] scale | proto=$PROTO | n=$N | rate=1 | duration=${DURATION}s | size=100"
    ./loadtest-bin \
      -mode scale \
      -proto "$PROTO" \
      -n "$N" \
      -rate 1 \
      -duration "$DURATION" \
      -size 100 \
      -target "$TARGET" \
      -room "$ROOM" \
      -alias-prefix "bench" \
      -output "$OUT_CSV" || echo "  [WARN] scale $PROTO n=$N failed"
    maybe_sleep
  done
done

# ─── Dimension 5: Throughput (msg rate sweep) ────────────────────────────────
hdr "Dimension 5: Throughput (msg rate sweep)  $(ts)"

for PROTO in tcp quic; do
  for RATE in $THROUGHPUT_RATES; do
    OUT_CSV="$OUTPUT_DIR/throughput/throughput_${PROTO}_${RATE}.csv"
    echo "  [$(ts)] throughput | proto=$PROTO | n=50 | rate=$RATE | duration=10 | size=100"
    ./loadtest-bin \
      -mode throughput \
      -proto "$PROTO" \
      -n 50 \
      -rates "$RATE" \
      -duration 10 \
      -size 100 \
      -target "$TARGET" \
      -room "$ROOM" \
      -alias-prefix "bench" \
      -output "$OUT_CSV" || echo "  [WARN] throughput $PROTO rate=$RATE failed"
    maybe_sleep
  done
done

# ─── Summary ─────────────────────────────────────────────────────────────────
hdr "Summary  $(ts)"

printf "  %-20s %-8s %-12s %-10s %s\n" \
  "DIMENSION" "PROTO" "PARAM" "STATUS" "FILE"
printf "  %-20s %-8s %-12s %-10s %s\n" \
  "---------" "-----" "-----" "------" "----"

for CSV in "$OUTPUT_DIR"/connection/dial_*.csv; do
  [[ -f "$CSV" ]] || continue
  BASENAME=$(basename "$CSV" .csv)
  PROTO=$(echo "$BASENAME" | cut -d'_' -f2)
  N=$(echo "$BASENAME" | cut -d'_' -f3)
  printf "  %-20s %-8s %-12s %-10s %s\n" \
    "connection" "$PROTO" "n=$N" "OK" "$CSV"
done

for CSV in "$OUTPUT_DIR"/latency/latency_*.csv; do
  [[ -f "$CSV" ]] || continue
  BASENAME=$(basename "$CSV" .csv)
  PROTO=$(echo "$BASENAME" | cut -d'_' -f2)
  SIZE=$(echo "$BASENAME" | cut -d'_' -f3)
  printf "  %-20s %-8s %-12s %-10s %s\n" \
    "latency" "$PROTO" "size=$SIZE" "OK" "$CSV"
done

for LOG in "$OUTPUT_DIR"/multistream/mplex_*.log; do
  [[ -f "$LOG" ]] || continue
  BASENAME=$(basename "$LOG" .log)
  PROTO=$(echo "$BASENAME" | cut -d'_' -f2)
  printf "  %-20s %-8s %-12s %-10s %s\n" \
    "multistream" "$PROTO" "n=$STREAM_COUNTS" "OK" "$LOG"
done

for CSV in "$OUTPUT_DIR"/scale/scale_*.csv; do
  [[ -f "$CSV" ]] || continue
  BASENAME=$(basename "$CSV" .csv)
  PROTO=$(echo "$BASENAME" | cut -d'_' -f2)
  N=$(echo "$BASENAME" | cut -d'_' -f3)
  printf "  %-20s %-8s %-12s %-10s %s\n" \
    "scale" "$PROTO" "n=$N" "OK" "$CSV"
done

for CSV in "$OUTPUT_DIR"/throughput/throughput_*.csv; do
  [[ -f "$CSV" ]] || continue
  BASENAME=$(basename "$CSV" .csv)
  PROTO=$(echo "$BASENAME" | cut -d'_' -f2)
  RATE=$(echo "$BASENAME" | cut -d'_' -f3)
  printf "  %-20s %-8s %-12s %-10s %s\n" \
    "throughput" "$PROTO" "rate=$RATE" "OK" "$CSV"
done

echo ""
echo "  Output directory: $OUTPUT_DIR"
echo "  Total benchmark runs completed: $RUN_COUNT"
sep
