#!/bin/bash
# repeat_study.sh — run the QUIC-vs-TCP comparative study N times for robust statistics.
#
# Wraps bench/compare_study.sh, invoking it REPEATS times and writing each run to a
# separate rep_<i> directory under a labelled root, so the per-run CSVs can be aggregated
# (mean / median / std / 95% CI) by bench/aggregate_repeats.py. Records provenance
# (git HEAD, timestamp, params) in <root>/RUN_INFO.txt.
#
# Designed to run unattended (nohup/detached) since a full set takes a while.
#
# Usage:
#   TARGET=63.185.9.233:41748 LABEL=baseline REPEATS=5 ./bench/repeat_study.sh
#   nohup env TARGET=63.185.9.233:41748 LABEL=baseline REPEATS=5 \
#        ./bench/repeat_study.sh > repeat_baseline.log 2>&1 &
#
# Env:
#   TARGET   (required) relay host:port
#   LABEL    (default: run) label for the result root: bench_results/repeats_<LABEL>/
#   REPEATS  (default: 5) number of full study repetitions
#   PAUSE_BETWEEN_REPS (default: 20) seconds to let the relay settle between repeats
#   plus any compare_study.sh env overrides (DIAL_PEERS, SCALE_PEERS, LAT_SIZES, THRU_RATES…)
set -uo pipefail

TARGET="${TARGET:-}"
LABEL="${LABEL:-run}"
REPEATS="${REPEATS:-5}"
PAUSE_BETWEEN_REPS="${PAUSE_BETWEEN_REPS:-20}"

if [[ -z "$TARGET" ]]; then echo "FATAL: set TARGET=host:port"; exit 1; fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

ROOT="bench_results/repeats_${LABEL}"
mkdir -p "$ROOT"

INFO="$ROOT/RUN_INFO.txt"
{
  echo "label:    $LABEL"
  echo "target:   $TARGET"
  echo "repeats:  $REPEATS"
  echo "started:  $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "git_head: $(git rev-parse --short HEAD 2>/dev/null)"
  echo "git_status:"
  git status --short 2>/dev/null | sed 's/^/  /'
  echo "sweeps (effective compare_study.sh defaults unless overridden by env):"
  echo "  DIAL_PEERS=${DIAL_PEERS:-10 50 100 200 500}"
  echo "  SCALE_PEERS=${SCALE_PEERS:-10 50 200 500 1000}  SCALE_DURATION=${SCALE_DURATION:-20}"
  echo "  LAT_SIZES=${LAT_SIZES:-64 256 1024 4096 16384 65536}  LAT_N=${LAT_N:-10}"
  echo "  THRU_RATES=${THRU_RATES:-1 5 10 50 100}  THRU_N=${THRU_N:-50}"
} > "$INFO"

echo "=== repeat_study: LABEL=$LABEL REPEATS=$REPEATS TARGET=$TARGET ==="
echo "    output root: $ROOT"

for i in $(seq 1 "$REPEATS"); do
  REP_DIR="$ROOT/rep_$i"
  echo ""
  echo "############################################################"
  echo "# REP $i / $REPEATS  ->  $REP_DIR   ($(date -u +%H:%M:%SZ))"
  echo "############################################################"
  # compare_study.sh honours OUTDIR + TARGET + the sweep env vars.
  OUTDIR="$REP_DIR" TARGET="$TARGET" ./bench/compare_study.sh
  rc=$?
  echo "rep $i exit=$rc" >> "$INFO"
  if [[ "$i" -lt "$REPEATS" ]]; then
    echo "--- settling ${PAUSE_BETWEEN_REPS}s before next repeat ---"
    sleep "$PAUSE_BETWEEN_REPS"
  fi
done

echo "finished: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$INFO"
echo ""
echo "=== DONE. $REPEATS repeats under $ROOT ==="
echo "Pull to local and aggregate:"
echo "  scp -r <host>:~/secure-p2p-messaging-quic/$ROOT bench_results/"
echo "  python3 bench/aggregate_repeats.py $ROOT"
