#!/bin/bash
TARGET="172.20.0.10"
echo "============================================"
echo "      RUNNING BENCHMARKS (10MB Transfer)    "
echo "============================================"

echo ""
echo "1. TCP + TLS Benchmark..."
echo "-------------------------"
go run tcp_bench.go client $TARGET 9000

echo ""
echo "-------------------------"
echo "2. QUIC Benchmark..."
echo "-------------------------"
go run quic_bench.go client $TARGET 9001

echo ""
echo "============================================"
echo "Done."
