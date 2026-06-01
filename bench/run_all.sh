#!/bin/bash
# bench/run_all.sh — automated benchmark runner for secure-p2p-messaging-quic
#
# Orchestrates application-level (loadtest) and raw transport benchmarks
# across multiple peer scales. Designed for client-side execution against
# a relay peer running in another region.
#
# Usage:
#   ./bench/run_all.sh [options]
#
# Options (override environment variables):
#   --target ADDR:PORT      Relay address (env: TARGET)
#   --proto quic|tcp        Protocol (env: PROTO, default: quic)
#   --room NAME             Room name (env: ROOM, default: benchmark)
#   --duration SEC          Per-test duration (env: DURATION, default: 30)
#   --rate RPS              Messages per second per peer (env: RATE, default: 1)
#   --size BYTES            Message size (env: SIZE, default: 100)
#   --output-dir DIR        Output directory (env: OUTPUT_DIR, default: ./bench_results)
#   --raw-target ADDR       Raw benchmark target host (env: RAW_TARGET)
#   --raw-tcp-port PORT     Raw TCP port (env: RAW_TCP_PORT)
#   --raw-quic-port PORT    Raw QUIC port (env: RAW_QUIC_PORT)
#   --skip-raw              Skip raw transport benchmarks (env: SKIP_RAW=1)
#   --help, -h              Show this help
#
# Examples:
#   TARGET=35.157.17.0:10000 ./bench/run_all.sh
#   ./bench/run_all.sh --target 35.157.17.0:10000 --proto quic --duration 60
set -euo pipefail

# ─── Helpers ─────────────────────────────────────────────────────────────────
sep()  { printf '=%.0s' {1..72}; printf '\n'; }
hdr()  { sep; printf "  %s\n" "$*"; sep; }
ts()   { date '+%Y-%m-%d %H:%M:%S'; }

show_help() {
cat <<EOF
bench/run_all.sh — automated benchmark runner

Application-level benchmarks (always run unless TARGET is missing):
  ./bench/run_all.sh --target 35.157.17.0:10000 --proto quic --duration 30

Environment variables (same names, UPPER_CASE):
  TARGET         Relay address:port (required)
  PROTO          quic or tcp (default: quic)
  ROOM           Room name (default: benchmark)
  DURATION       Seconds per test (default: 30)
  RATE           Messages/sec per peer (default: 1)
  SIZE           Message payload size in bytes (default: 100)
  OUTPUT_DIR     Where CSVs are written (default: ./bench_results)

Raw transport benchmarks (optional):
  RAW_TARGET     Host for raw_mini benchmarks
  RAW_TCP_PORT   TCP port for raw_mini server
  RAW_QUIC_PORT  QUIC port for raw_mini server
  SKIP_RAW       Set to 1 to skip raw benchmarks

EOF
}

# ─── Defaults ────────────────────────────────────────────────────────────────
TARGET="${TARGET:-}"
PROTO="${PROTO:-quic}"
ROOM="${ROOM:-benchmark}"
DURATION="${DURATION:-30}"
RATE="${RATE:-1}"
SIZE="${SIZE:-100}"
OUTPUT_DIR="${OUTPUT_DIR:-./bench_results}"
RAW_TARGET="${RAW_TARGET:-}"
RAW_TCP_PORT="${RAW_TCP_PORT:-}"
RAW_QUIC_PORT="${RAW_QUIC_PORT:-}"
SKIP_RAW="${SKIP_RAW:-0}"
RAW_LOG=""

# ─── CLI arg parsing ───────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)       TARGET="$2"; shift 2 ;;
    --proto)        PROTO="$2";  shift 2 ;;
    --room)         ROOM="$2";   shift 2 ;;
    --duration)     DURATION="$2"; shift 2 ;;
    --rate)         RATE="$2";   shift 2 ;;
    --size)         SIZE="$2";   shift 2 ;;
    --output-dir)   OUTPUT_DIR="$2"; shift 2 ;;
    --raw-target)   RAW_TARGET="$2"; shift 2 ;;
    --raw-tcp-port) RAW_TCP_PORT="$2"; shift 2 ;;
    --raw-quic-port)RAW_QUIC_PORT="$2"; shift 2 ;;
    --skip-raw)     SKIP_RAW=1; shift ;;
    --help|-h)      show_help; exit 0 ;;
    --)             shift; break ;;
    *) echo "Unknown option: $1"; show_help; exit 1 ;;
  esac
done

# ─── Resolve repo root ───────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# ─── Pre-flight checks ───────────────────────────────────────────────────────
hdr "Pre-flight checks  $(ts)"

if [[ -z "${TARGET}" ]]; then
  echo "FATAL: TARGET is required (set env var or --target)"
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

echo "  Checking reachability of $TARGET ..."
HOST="${TARGET%%:*}"
PORT="${TARGET##*:}"
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

mkdir -p "$OUTPUT_DIR"
echo "  → OUTPUT_DIR: $(cd "$OUTPUT_DIR" && pwd)"

# ─── Application-level benchmarks ────────────────────────────────────────────
hdr "Application Benchmarks  $(ts)"
echo "  Config: PROTO=$PROTO  ROOM=$ROOM  DURATION=${DURATION}s  RATE=${RATE}  SIZE=${SIZE}"

SCALES=(50 200 500 1000)
CSV_FILES=()

for N in "${SCALES[@]}"; do
  hdr "Loadtest — $PROTO | $N peers | ${RATE} msg/s | ${DURATION}s | ${SIZE} bytes"

  OUT_CSV="$OUTPUT_DIR/loadtest_${PROTO}_${N}.csv"
  CSV_FILES+=("$OUT_CSV")

  echo "  Running: ./loadtest-bin -n $N -target $TARGET -room $ROOM -rate $RATE -duration $DURATION -proto $PROTO -size $SIZE -alias-prefix bench -output $OUT_CSV"

  EXIT_CODE=0
  ./loadtest-bin \
    -n "$N" \
    -target "$TARGET" \
    -room "$ROOM" \
    -rate "$RATE" \
    -duration "$DURATION" \
    -proto "$PROTO" \
    -size "$SIZE" \
    -alias-prefix "bench" \
    -output "$OUT_CSV" || EXIT_CODE=$?

  if [[ $EXIT_CODE -ne 0 ]]; then
    echo "  [ERROR] loadtest exited with code $EXIT_CODE for N=$N"
  else
    echo "  → Results written to: $OUT_CSV"
  fi

  if [[ "$N" != "${SCALES[${#SCALES[@]}-1]}" ]]; then
    echo "  Sleeping 5s to let the relay recover ..."
    sleep 5
  fi
done

# ─── Concatenate per-scale CSVs ──────────────────────────────────────────────
ALL_CSV="$OUTPUT_DIR/loadtest_${PROTO}_all.csv"
hdr "Aggregating results  $(ts)"
FIRST=true
: > "$ALL_CSV"
for CSV in "${CSV_FILES[@]}"; do
  if [[ ! -f "$CSV" ]]; then
    echo "  [WARN] Skipping missing CSV: $CSV"
    continue
  fi
  if $FIRST; then
    cat "$CSV" >> "$ALL_CSV"
    FIRST=false
  else
    tail -n +2 "$CSV" >> "$ALL_CSV"
  fi
done

if $FIRST; then
  echo "  [WARN] No per-scale CSVs were produced. Nothing to aggregate."
else
  echo "  → Aggregated CSV: $ALL_CSV ($(wc -l < "$ALL_CSV" | tr -d ' ') lines)"
fi

# ─── Raw transport benchmarks (optional) ─────────────────────────────────────
if [[ "$SKIP_RAW" == "0" || "$SKIP_RAW" == "false" || "$SKIP_RAW" == "" ]]; then
  if [[ -n "$RAW_TARGET" && -n "$RAW_TCP_PORT" && -n "$RAW_QUIC_PORT" ]]; then
    RAW_LOG="$OUTPUT_DIR/raw_transport.log"
    hdr "Raw Transport Benchmarks  $(ts)"
    echo "  RAW_TARGET=$RAW_TARGET  TCP_PORT=$RAW_TCP_PORT  QUIC_PORT=$RAW_QUIC_PORT"
    echo "  Logging to: $RAW_LOG"
    : > "$RAW_LOG"

    # multiplex_bench.go TCP
    echo "[$(ts)] multiplex_bench.go -proto tcp -n 8" >> "$RAW_LOG"
    if go run raw_mini/multiplex_bench.go -proto tcp -n 8 -addr "$RAW_TARGET" -tcpport "$RAW_TCP_PORT" >> "$RAW_LOG" 2>&1; then
      echo "  ✓ multiplex_bench.go TCP (n=8)"
    else
      echo "  ✗ multiplex_bench.go TCP (n=8) failed — see $RAW_LOG"
    fi

    # multiplex_bench.go QUIC
    echo "[$(ts)] multiplex_bench.go -proto quic -n 8" >> "$RAW_LOG"
    if go run raw_mini/multiplex_bench.go -proto quic -n 8 -addr "$RAW_TARGET" -quicport "$RAW_QUIC_PORT" >> "$RAW_LOG" 2>&1; then
      echo "  ✓ multiplex_bench.go QUIC (n=8)"
    else
      echo "  ✗ multiplex_bench.go QUIC (n=8) failed — see $RAW_LOG"
    fi

    # quic_diagnostic.go -wan
    echo "[$(ts)] quic_diagnostic.go -wan client $RAW_TARGET $RAW_QUIC_PORT 1024" >> "$RAW_LOG"
    if go run raw_mini/quic_diagnostic.go -wan client "$RAW_TARGET" "$RAW_QUIC_PORT" 1024 >> "$RAW_LOG" 2>&1; then
      echo "  ✓ quic_diagnostic.go -wan"
    else
      echo "  ✗ quic_diagnostic.go -wan failed — see $RAW_LOG"
    fi

    # quic_diagnostic.go -reuse
    echo "[$(ts)] quic_diagnostic.go -reuse client $RAW_TARGET $RAW_QUIC_PORT" >> "$RAW_LOG"
    if go run raw_mini/quic_diagnostic.go -reuse client "$RAW_TARGET" "$RAW_QUIC_PORT" >> "$RAW_LOG" 2>&1; then
      echo "  ✓ quic_diagnostic.go -reuse"
    else
      echo "  ✗ quic_diagnostic.go -reuse failed — see $RAW_LOG"
    fi
  else
    echo "  [INFO] Raw benchmarks skipped (set RAW_TARGET, RAW_TCP_PORT, RAW_QUIC_PORT to enable)"
  fi
else
  echo "  [INFO] Raw benchmarks skipped (SKIP_RAW set)"
fi

# ─── Summary ─────────────────────────────────────────────────────────────────
hdr "Summary  $(ts)"

if [[ -s "$ALL_CSV" ]]; then
  printf "  %-6s %6s %10s %10s %10s %10s %14s %12s %10s\n" \
    "PROTO" "PEERS" "DIAL_AVG" "DIAL_P95" "JOIN_AVG" "JOIN_P95" "THRU_MSG/s" "THRU_KB/s" "P95_RTT"
  printf "  %-6s %6s %10s %10s %10s %10s %14s %12s %10s\n" \
    "" "" "(ms)" "(ms)" "(ms)" "(ms)" "" "" "(ms)"

  tail -n +2 "$ALL_CSV" | while IFS=',' read -r _TS PROTO_VAL N_VAL _RATE _DUR _SIZE _TARGET DIAL_AVG DIAL_P95 JOIN_AVG JOIN_P95 _SENT _RECV THRU_MSG THRU_KB _MIN_RTT _P50_RTT P95_RTT _P99_RTT _MAX_RTT _ERRS _DISC; do
    printf "  %-6s %6s %10s %10s %10s %10s %14s %12s %10s\n" \
      "$PROTO_VAL" "$N_VAL" "$DIAL_AVG" "$DIAL_P95" "$JOIN_AVG" "$JOIN_P95" "$THRU_MSG" "$THRU_KB" "$P95_RTT"
  done
else
  echo "  [WARN] No aggregated data available for summary."
fi

echo ""
echo "  Output files:"
if [[ -f "$ALL_CSV" ]]; then
  echo "    $ALL_CSV"
fi
for CSV in "${CSV_FILES[@]}"; do
  if [[ -f "$CSV" ]]; then
    echo "    $CSV"
  fi
done
if [[ -f "$RAW_LOG" ]]; then
  echo "    $RAW_LOG"
fi

sep
