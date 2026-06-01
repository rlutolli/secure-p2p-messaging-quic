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
# Designed to run unattended (e.g. under nohup) and to produce a TRUSTWORTHY
# summary: a test is only marked OK when the loadtest exited cleanly AND the
# CSV reports zero real errors. Expected connection-teardown noise is filtered
# from the console (full output is preserved in per-test .raw.log files).
#
# Usage:
#   ./bench/run_full.sh --target 63.185.9.233:41748 --room benchmark [options]
#   nohup ./bench/run_full.sh --target 63.185.9.233:41748 > bench.log 2>&1 &
#
# Options (override environment variables):
#   --target ADDR:PORT      Relay address (env: TARGET, required)
#   --room NAME             Room name (env: ROOM, default: benchmark)
#   --duration SEC          Duration per test (env: DURATION, default: 30)
#   --output-dir DIR        Output directory (env: OUTPUT_DIR,
#                           default: ./bench_results/run_$(date +%Y%m%d_%H%M%S))
#   --inter-test-delay SEC  Pause between tests (env: INTER_TEST_DELAY, default: 10)
#   --skip-raw              Skip raw transport benchmarks (env: SKIP_RAW=1)
#   --help, -h              Show this help
#
# Additional environment variables:
#   RELAY_HOST / RELAY_PORT   Health-check target (default: derived from TARGET)
#   HEALTH_RETRIES            Relay reachability retries before a test (default: 10)
#   INTER_TEST_DELAY          Seconds to pause between tests (default: 10; use 30 for n=1000)
#   DIAL_STAGGER              Passed to loadtest -dial-stagger (e.g. 10ms; default: auto)
#   DIAL_RETRIES              Passed to loadtest -dial-retries (default: loadtest default)
#   PROTOS                    Space-separated protocols to test (default: "tcp quic")
#   DELIVERY_THRESHOLD        Min msgs_recv/msgs_sent ratio before a DEGRADED mark (default: 0.90)
#
# Examples:
#   TARGET=63.185.9.233:41748 ./bench/run_full.sh
#   ./bench/run_full.sh --target 63.185.9.233:41748 --duration 60 --inter-test-delay 30 --skip-raw

# pipefail so a failing stage in a pipe is visible; we deliberately do NOT use
# `set -e` so a single failed test does not abort the whole unattended suite —
# failures are recorded and surfaced in the summary instead.
set -uo pipefail

# ─── Helpers ─────────────────────────────────────────────────────────────────
sep()  { printf '=%.0s' {1..72}; printf '\n'; }
hdr()  { sep; printf "  %s\n" "$*"; sep; }
ts()   { date '+%Y-%m-%d %H:%M:%S'; }
log()  { printf "  [%s] %s\n" "$(ts)" "$*"; }

show_help() {
  sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
}

# ─── Defaults ──────────────────────────────────────────────────────────────────
TARGET="${TARGET:-}"
ROOM="${ROOM:-benchmark}"
DURATION="${DURATION:-30}"
OUTPUT_DIR="${OUTPUT_DIR:-}"
SKIP_RAW="${SKIP_RAW:-0}"
INTER_TEST_DELAY="${INTER_TEST_DELAY:-10}"
HEALTH_RETRIES="${HEALTH_RETRIES:-10}"
DIAL_STAGGER="${DIAL_STAGGER:-}"
DIAL_RETRIES="${DIAL_RETRIES:-}"
PROTOS="${PROTOS:-tcp quic}"
DELIVERY_THRESHOLD="${DELIVERY_THRESHOLD:-0.90}"

# Scale parameters (override with env vars to dial up/down)
DIAL_PEERS="${DIAL_PEERS:-10 50 100 200 500}"
SCALE_PEERS="${SCALE_PEERS:-50 200 500 1000}"
THROUGHPUT_RATES="${THROUGHPUT_RATES:-1 5 10 50 100}"
LATENCY_SIZES="${LATENCY_SIZES:-64 256 1024 4096 16384 65536}"
STREAM_COUNTS="${STREAM_COUNTS:-8}"

# ─── CLI arg parsing ───────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)            TARGET="$2"; shift 2 ;;
    --room)              ROOM="$2";   shift 2 ;;
    --duration)          DURATION="$2"; shift 2 ;;
    --output-dir)        OUTPUT_DIR="$2"; shift 2 ;;
    --inter-test-delay)  INTER_TEST_DELAY="$2"; shift 2 ;;
    --skip-raw)          SKIP_RAW=1; shift ;;
    --help|-h)           show_help; exit 0 ;;
    --)                  shift; break ;;
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

# ─── Noise classification ──────────────────────────────────────────────────────
# Benign strings appear during normal connection teardown and are filtered from
# the console. Real strings indicate genuine failures and are always shown.
NOISE_RE='Application error 0x0|use of closed network connection|context canceled|server closed|: EOF|\bEOF\b|loadtest done|stream accept timeout'
REAL_RE='CONNECTION_REFUSED|connection reset by peer|connection refused|i/o timeout|\btimeout\b|no route to host|too many open files|broken pipe|handshake'

# ─── Derived health-check target ───────────────────────────────────────────────
RELAY_HOST="${RELAY_HOST:-${TARGET%%:*}}"
RELAY_PORT="${RELAY_PORT:-${TARGET##*:}}"

# tcp_probe HOST PORT — returns 0 if a TCP connection succeeds within 5s.
tcp_probe() {
  local host="$1" port="$2"
  if command -v nc >/dev/null 2>&1; then
    nc -z -w5 "$host" "$port" >/dev/null 2>&1
  else
    timeout 5 bash -c "echo >/dev/tcp/$host/$port" >/dev/null 2>&1
  fi
}

# wait_for_relay — probe the relay until reachable or HEALTH_RETRIES exhausted.
# Returns 0 if reachable, 1 otherwise.
wait_for_relay() {
  local attempt
  for (( attempt=1; attempt<=HEALTH_RETRIES; attempt++ )); do
    if tcp_probe "$RELAY_HOST" "$RELAY_PORT"; then
      return 0
    fi
    log "relay $RELAY_HOST:$RELAY_PORT not ready (attempt $attempt/$HEALTH_RETRIES); waiting 3s ..."
    sleep 3
  done
  return 1
}

# ─── Result tracking ───────────────────────────────────────────────────────────
RUN_COUNT=0
FAIL_COUNT=0
DEGRADED_COUNT=0
RESULTS_TSV=""   # populated after OUTPUT_DIR exists

# csv_field FILE COLUMN_NAME — print the value of COLUMN_NAME from the first
# data row of a Go csv.Writer file. Returns empty string if missing.
csv_field() {
  local file="$1" col="$2"
  [[ -f "$file" ]] || return 0
  awk -F, -v want="$col" '
    NR==1 { for (i=1;i<=NF;i++) { h=$i; gsub(/^[ \t]+|[ \t]+$/,"",h); if (h==want) c=i } }
    NR==2 { if (c) { v=$c; gsub(/^[ \t]+|[ \t]+$/,"",v); print v } }
  ' "$file"
}

# first_present FILE COL1 COL2 — value of whichever column exists (sent/recv
# differ between scale and throughput CSVs).
first_present() {
  local file="$1" a="$2" b="$3" v
  v="$(csv_field "$file" "$a")"
  [[ -n "$v" ]] && { echo "$v"; return 0; }
  csv_field "$file" "$b"
}

record_result() {
  # dim proto param status detail file
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" "$6" >> "$RESULTS_TSV"
  case "$4" in
    FAIL)     FAIL_COUNT=$((FAIL_COUNT+1)) ;;
    DEGRADED) DEGRADED_COUNT=$((DEGRADED_COUNT+1)) ;;
  esac
}

# inter_test_pause — pause between tests and confirm the relay is back before
# the next test starts (prevents cascading failures while the relay reclaims
# goroutines/ports after a large test).
inter_test_pause() {
  RUN_COUNT=$((RUN_COUNT+1))
  log "pausing ${INTER_TEST_DELAY}s for relay recovery ..."
  sleep "$INTER_TEST_DELAY"
  if ! wait_for_relay; then
    log "WARNING: relay $RELAY_HOST:$RELAY_PORT still unreachable; subsequent tests may FAIL"
  fi
}

# run_loadtest DIM PROTO PARAM OUTFILE -- <loadtest args...>
# Executes the loadtest, filters benign noise to the console, preserves raw
# stderr, then derives a trustworthy OK/FAIL/DEGRADED status from the CSV.
run_loadtest() {
  local dim="$1" proto="$2" param="$3" outfile="$4"; shift 4
  local raw="${outfile%.csv}.raw.log"

  local extra=()
  [[ -n "$DIAL_STAGGER" ]] && extra+=(-dial-stagger "$DIAL_STAGGER")
  [[ -n "$DIAL_RETRIES" ]] && extra+=(-dial-retries "$DIAL_RETRIES")

  log "$dim | proto=$proto | $param"
  ./loadtest-bin "$@" "${extra[@]+"${extra[@]}"}" -output "$outfile" 2> "$raw"
  local rc=$?

  # Show only non-benign stderr lines on the console.
  if [[ -s "$raw" ]]; then
    grep -vE "$NOISE_RE" "$raw" 1>&2 || true
  fi

  # Derive status from the CSV (authoritative), not from console noise.
  local errors sent recv status detail
  errors="$(csv_field "$outfile" errors_total)"
  sent="$(first_present "$outfile" total_msgs_sent msgs_sent)"
  recv="$(first_present "$outfile" total_msgs_recv msgs_recv)"

  status="OK"
  detail="errors=${errors:-?}"

  if [[ $rc -ne 0 || ! -f "$outfile" ]]; then
    status="FAIL"
    detail="exit=$rc (no/failed output)"
  elif [[ -n "$errors" && "$errors" -gt 0 ]]; then
    status="FAIL"
    detail="errors=$errors"
  elif [[ -n "$sent" && "$sent" -gt 0 && -n "$recv" ]]; then
    # delivery ratio check for message-bearing modes
    local ratio
    ratio="$(awk -v r="$recv" -v s="$sent" 'BEGIN{ if (s>0) printf "%.4f", r/s; else print "0" }')"
    detail="errors=${errors:-0} delivery=${recv}/${sent} (${ratio})"
    if awk -v r="$ratio" -v t="$DELIVERY_THRESHOLD" 'BEGIN{ exit !(r < t) }'; then
      status="DEGRADED"
    fi
  fi

  if [[ "$status" != "OK" ]]; then
    log "  → $status: $detail  (raw: $raw)"
  else
    log "  → OK: $detail"
  fi
  record_result "$dim" "$proto" "$param" "$status" "$detail" "$outfile"
}

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

echo "  Target:           $TARGET"
echo "  Health-check:     $RELAY_HOST:$RELAY_PORT (retries: $HEALTH_RETRIES)"
echo "  Inter-test delay: ${INTER_TEST_DELAY}s"
echo "  Protocols:        $PROTOS"
[[ -n "$DIAL_STAGGER" ]] && echo "  Dial stagger:     $DIAL_STAGGER"
[[ -n "$DIAL_RETRIES" ]] && echo "  Dial retries:     $DIAL_RETRIES"

echo "  Checking relay reachability ..."
if wait_for_relay; then
  echo "  ✓ relay $RELAY_HOST:$RELAY_PORT reachable"
else
  echo "  ⚠ WARNING: relay $RELAY_HOST:$RELAY_PORT unreachable after $HEALTH_RETRIES attempts; continuing anyway"
fi

mkdir -p "$OUTPUT_DIR"/{connection,latency,multistream,scale,throughput}
RESULTS_TSV="$OUTPUT_DIR/results.tsv"
: > "$RESULTS_TSV"
echo "  → OUTPUT_DIR: $(cd "$OUTPUT_DIR" && pwd)"

# ─── Dimension 1: Connection (dial time at scale) ────────────────────────────
hdr "Dimension 1: Connection (dial time at scale)  $(ts)"
for PROTO in $PROTOS; do
  for N in $DIAL_PEERS; do
    run_loadtest "connection" "$PROTO" "n=$N" \
      "$OUTPUT_DIR/connection/dial_${PROTO}_${N}.csv" \
      -mode dial -proto "$PROTO" -n "$N" -target "$TARGET"
    inter_test_pause
  done
done

# ─── Dimension 2: Message Latency (RTT by size) ──────────────────────────────
hdr "Dimension 2: Message Latency (RTT by size)  $(ts)"
for PROTO in $PROTOS; do
  for SIZE in $LATENCY_SIZES; do
    run_loadtest "latency" "$PROTO" "size=$SIZE" \
      "$OUTPUT_DIR/latency/latency_${PROTO}_${SIZE}.csv" \
      -mode latency -proto "$PROTO" -n 10 -sizes "$SIZE" -duration 5 \
      -target "$TARGET" -room "$ROOM"
    inter_test_pause
  done
done

# ─── Dimension 3: Multi-Stream (raw transport) ───────────────────────────────
if [[ "$SKIP_RAW" != "1" && "$SKIP_RAW" != "true" ]]; then
  hdr "Dimension 3: Multi-Stream (raw transport)  $(ts)"
  for PROTO in $PROTOS; do
    OUT_LOG="$OUTPUT_DIR/multistream/mplex_${PROTO}.log"
    log "multistream | proto=$PROTO | n=$STREAM_COUNTS | addr=$RELAY_HOST | port=$RELAY_PORT"
    if go run raw_mini/multiplex_bench.go \
        -proto "$PROTO" -n "$STREAM_COUNTS" \
        -addr "$RELAY_HOST" -tcpport "$RELAY_PORT" -quicport "$RELAY_PORT" \
        > "$OUT_LOG" 2>&1; then
      if grep -qE "$REAL_RE" "$OUT_LOG"; then
        log "  → DEGRADED: completed but real errors present (see $OUT_LOG)"
        record_result "multistream" "$PROTO" "n=$STREAM_COUNTS" "DEGRADED" "errors in log" "$OUT_LOG"
      else
        log "  → OK"
        record_result "multistream" "$PROTO" "n=$STREAM_COUNTS" "OK" "completed" "$OUT_LOG"
      fi
    else
      log "  → FAIL: multistream $PROTO (see $OUT_LOG)"
      record_result "multistream" "$PROTO" "n=$STREAM_COUNTS" "FAIL" "nonzero exit" "$OUT_LOG"
    fi
    inter_test_pause
  done
else
  echo "  [INFO] Raw benchmarks skipped (--skip-raw or SKIP_RAW set)"
fi

# ─── Dimension 4: Scale (many peers) ───────────────────────────────────────────
hdr "Dimension 4: Scale (many peers)  $(ts)"
for PROTO in $PROTOS; do
  for N in $SCALE_PEERS; do
    run_loadtest "scale" "$PROTO" "n=$N" \
      "$OUTPUT_DIR/scale/scale_${PROTO}_${N}.csv" \
      -mode scale -proto "$PROTO" -n "$N" -rate 1 -duration "$DURATION" -size 100 \
      -target "$TARGET" -room "$ROOM" -alias-prefix "bench"
    inter_test_pause
  done
done

# ─── Dimension 5: Throughput (msg rate sweep) ────────────────────────────────
hdr "Dimension 5: Throughput (msg rate sweep)  $(ts)"
for PROTO in $PROTOS; do
  for RATE in $THROUGHPUT_RATES; do
    run_loadtest "throughput" "$PROTO" "rate=$RATE" \
      "$OUTPUT_DIR/throughput/throughput_${PROTO}_${RATE}.csv" \
      -mode throughput -proto "$PROTO" -n 50 -rates "$RATE" -duration 10 -size 100 \
      -target "$TARGET" -room "$ROOM" -alias-prefix "bench"
    inter_test_pause
  done
done

# ─── Summary ─────────────────────────────────────────────────────────────────
hdr "Summary  $(ts)"
printf "  %-12s %-6s %-12s %-10s %s\n" "DIMENSION" "PROTO" "PARAM" "STATUS" "DETAIL"
printf "  %-12s %-6s %-12s %-10s %s\n" "---------" "-----" "-----" "------" "------"
while IFS=$'\t' read -r dim proto param status detail _file; do
  printf "  %-12s %-6s %-12s %-10s %s\n" "$dim" "$proto" "$param" "$status" "$detail"
done < "$RESULTS_TSV"

echo ""
echo "  Output directory: $OUTPUT_DIR"
echo "  Total tests:      $RUN_COUNT"
echo "  Failures:         $FAIL_COUNT"
echo "  Degraded:         $DEGRADED_COUNT"
sep

if [[ "$FAIL_COUNT" -gt 0 ]]; then
  echo "  RESULT: FAIL ($FAIL_COUNT real failures). Inspect the *.raw.log files."
  exit 1
elif [[ "$DEGRADED_COUNT" -gt 0 ]]; then
  echo "  RESULT: DEGRADED ($DEGRADED_COUNT below delivery threshold $DELIVERY_THRESHOLD)."
  exit 2
else
  echo "  RESULT: OK — all tests passed with zero real errors."
  exit 0
fi
