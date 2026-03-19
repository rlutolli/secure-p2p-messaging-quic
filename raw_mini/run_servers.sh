#!/bin/bash
# run_servers.sh – Start TCP+TLS and QUIC benchmark servers on this node.
#
# Run this on node-b BEFORE running bench.sh on node-a.
#
# Both servers use TLS 1.3 and write session keys to tls_keys.log so that
# Wireshark on the macOS host can decrypt all traffic on bridge100.
#
# Ports:
#   9000  – TCP+TLS  (tcp_raw.go)
#   9001  – QUIC     (quic_diagnostic.go)

export PATH=$PATH:/usr/local/go/bin

KEYLOG=/home/ubuntu/p2p/tls_keys.log
RAWMINI_DIR="$(cd "$(dirname "$0")" && pwd)"

# Kill any stale server processes from a previous run
pkill -f "tcp_raw.go"        2>/dev/null || true
pkill -f "quic_diagnostic.go" 2>/dev/null || true
sleep 1

trap "kill 0" EXIT

echo ">>> Starting TCP+TLS server  on :9000  (key log: $KEYLOG)"
SSLKEYLOGFILE=$KEYLOG go run "$RAWMINI_DIR/tcp_raw.go" server 9000 &

echo ">>> Starting QUIC server     on :9001  (key log: $KEYLOG)"
SSLKEYLOGFILE=$KEYLOG go run "$RAWMINI_DIR/quic_diagnostic.go" server 9001 &

echo ""
echo "Both servers running. Press Ctrl+C to stop."
wait
