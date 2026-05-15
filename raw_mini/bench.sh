#!/bin/bash
# bench.sh – TCP+TLS vs QUIC benchmark  (6 scenarios, WAN-realistic conditions)
#
# Run this on node-a.  node-b must be running tcp_server and quic_server.
#
# ╔══════════════════════════════════════════════════════════════════════════╗
# ║  SCENARIO  │  tc netem condition        │  What QUIC advantage is shown ║
# ╠══════════════════════════════════════════════════════════════════════════╣
# ║  1 – LAN   │  none                      │  QUIC 0-RTT vs TCP 2-RTT HS   ║
# ║  2 – WAN   │  delay 40ms                │  QUIC 1-RTT HS vs TCP 2-RTT   ║
# ║  3 – Lossy │  delay 20ms loss 1%        │  QUIC loss recovery < TCP RTO ║
# ║  4 – HiLos │  delay 30ms loss 3%        │  Same, more visible           ║
# ║  5 – Mplex │  delay 20ms loss 1%        │  QUIC independent streams     ║
# ║  6 – Burst │  delay 40ms                │  QUIC 0-RTT vs TCP 3-RTT/conn ║
# ╚══════════════════════════════════════════════════════════════════════════╝
#
# Usage:
#   cd /home/ubuntu/p2p/raw_mini
#   bash bench.sh
#
# Requires sudo (for tc qdisc).  The virtiofs mount provides access to the
# shared tls_keys.log file (readable on macOS at /tmp/p2p/tls_keys.log).

set -e
export PATH=$PATH:/usr/local/go/bin

TARGET=192.168.2.5
TCP_PORT=9000
QUIC_PORT=9001
KEYLOG=/tmp/tls_keys.log
DIR="$(cd "$(dirname "$0")" && pwd)"

# Auto-detect the interface used to reach node-b
IFACE=$(ip route get "$TARGET" | grep -oP 'dev \K\S+' | head -1)

# Truncate key log so each run starts clean (shared file via virtiofs)
> "$KEYLOG"

cd "$DIR"

# ─── Helpers ─────────────────────────────────────────────────────────────────

sep()  { printf "\n"; printf '═%.0s' {1..62}; printf "\n"; }
hdr()  { sep; printf "  %s\n" "$*"; sep; printf "\n"; }

netem_apply() {
    # Remove any existing qdisc first (ignore error if none set)
    sudo tc qdisc del dev "$IFACE" root 2>/dev/null || true
    if [ -n "$1" ]; then
        sudo tc qdisc add dev "$IFACE" root netem $1
        printf "[netem] %-16s → %s\n\n" "$IFACE" "$1"
    else
        printf "[netem] cleared (LAN baseline)\n\n"
    fi
}

netem_clear() {
    sudo tc qdisc del dev "$IFACE" root 2>/dev/null || true
}

TCP=/home/ubuntu/tcp_client
QUIC=/home/ubuntu/quic_client
MPLEX=/home/ubuntu/multiplex_client

# ─── Pre-flight: build client binaries ──────────────────────────────────────
hdr "Building client binaries on $(hostname)"
# Build from virtiofs dir if available, otherwise use existing binaries
if [ -r /home/ubuntu/p2p/raw_mini/tcp_raw.go ]; then
    (cd /home/ubuntu/p2p/raw_mini && /usr/local/go/bin/go build -o "$TCP"   tcp_raw.go)
    (cd /home/ubuntu/p2p/raw_mini && /usr/local/go/bin/go build -o "$QUIC"  quic_diagnostic.go)
    (cd /home/ubuntu/p2p/raw_mini && /usr/local/go/bin/go build -o "$MPLEX" multiplex_bench.go)
else
    echo "  [skipping build — using existing binaries]"
    [ -x "$TCP" ]   || { echo "FATAL: $TCP missing"; exit 1; }
    [ -x "$QUIC" ]  || { echo "FATAL: $QUIC missing"; exit 1; }
    [ -x "$MPLEX" ] || { echo "FATAL: $MPLEX missing"; exit 1; }
fi
printf "  tcp_client      → %s\n" "$TCP"
printf "  quic_client     → %s\n" "$QUIC"
printf "  multiplex_client→ %s\n" "$MPLEX"
printf "\n  Client : $(hostname)  ($(hostname -I | awk '{print $1}'))\n"
printf "  Server : %s\n" "$TARGET"
printf "  Key log: %s\n" "$KEYLOG"
printf "  Interface: %s\n\n" "$IFACE"

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 1 – LAN Baseline (no shaping)
# Measures FULL handshake + echo on a fresh connection per size.
# QUIC uses 0-RTT (after a warmup) while TCP pays a full TCP+TLS handshake.
# Even on clean LAN, QUIC's 0-RTT eliminates handshake latency.
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 1: LAN Baseline  [no shaping – QUIC 0-RTT vs TCP full handshake]"
netem_apply ""

for SIZE in 64 100 5000; do
    echo "--- S1 TCP  SIZE=$SIZE ---"
    SSLKEYLOGFILE="$KEYLOG" "$TCP"  -wan client "$TARGET" "$TCP_PORT" "$SIZE"
    echo ""
    echo "--- S1 QUIC SIZE=$SIZE ---"
    SSLKEYLOGFILE="$KEYLOG" "$QUIC" -wan client "$TARGET" "$QUIC_PORT" "$SIZE"
    echo ""
done
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 2 – WAN Latency  (delay 40 ms)
# Measures FULL handshake + echo.  Timer starts before Dial.
#
# TCP+TLS: TCP SYN (1 RTT) + TLS 1.3 ClientHello/ServerHello (1 RTT) + data (1 RTT)
#          → DIAL_MS ≈ 80   TOTAL_MS ≈ 120 ms
#
# QUIC:    QUIC Initial + Handshake coalesced into 1 RTT → DIAL_MS ≈ 40
#          Then data echo → TOTAL_MS ≈ 80 ms
#
# QUIC saves 1 RTT (40 ms) on handshake alone.  With 0-RTT on reconnect
# (see Scenario 6) the saving grows to 2 RTTs.
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 2: WAN Handshake  [delay 40ms – QUIC 1-RTT HS vs TCP 2-RTT HS]"
netem_apply "delay 40ms"

echo "--- S2 TCP  (fresh conn, handshake measured) ---"
SSLKEYLOGFILE="$KEYLOG" "$TCP"  -wan client "$TARGET" "$TCP_PORT"  1024
echo ""
echo "--- S2 QUIC (fresh conn, handshake measured) ---"
SSLKEYLOGFILE="$KEYLOG" "$QUIC" -wan client "$TARGET" "$QUIC_PORT" 1024
echo ""
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 3 – Lossy Link  (delay 20 ms, 1% loss)
# Persistent connection, full size sweep.
#
# Under packet loss on a persistent connection:
#   TCP:  retransmit timer (RTO) ≥ 200 ms on Linux → a single dropped data
#         segment or ACK stalls ALL subsequent data for ≥ 200 ms.
#   QUIC: loss detected via packet-number gaps in 1.5×SRTT ≈ 30 ms; only the
#         affected stream-frame is retransmitted; others are unblocked.
#
# With enough messages, loss events become visible in RTT spikes.
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 3: Lossy Link  [delay 20ms loss 1% – QUIC fast recovery vs TCP RTO 200ms]"
netem_apply "delay 20ms loss 1%"

echo "--- S3 TCP  (persistent conn, all sizes) ---"
SSLKEYLOGFILE="$KEYLOG" "$TCP"  -reuse client "$TARGET" "$TCP_PORT"
echo ""
echo "--- S3 QUIC (persistent conn, all sizes) ---"
SSLKEYLOGFILE="$KEYLOG" "$QUIC" -reuse client "$TARGET" "$QUIC_PORT"
echo ""
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 4 – High Loss  (delay 30 ms, 3% loss)
# Same as Scenario 3 but with 3% loss: loss events occur in nearly every run,
# making the TCP RTO stall clearly visible in the raw RTT numbers.
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 4: High Loss  [delay 30ms loss 3% – TCP RTO stalls vs QUIC NACK recovery]"
netem_apply "delay 30ms loss 3%"

echo "--- S4 TCP  (persistent conn, all sizes) ---"
SSLKEYLOGFILE="$KEYLOG" "$TCP"  -reuse client "$TARGET" "$TCP_PORT"
echo ""
echo "--- S4 QUIC (persistent conn, all sizes) ---"
SSLKEYLOGFILE="$KEYLOG" "$QUIC" -reuse client "$TARGET" "$QUIC_PORT"
echo ""
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 5 – Multiplexed Streams  (delay 20 ms, 1% loss)
# 8 goroutines launched simultaneously.
#
# QUIC: 1 shared connection (0-RTT after warmup), 8 independent streams.
#   - Connection setup cost paid ONCE (0-RTT ≈ instant via DialAddrEarly).
#   - A lost packet on stream i does NOT stall streams j≠i.
#   - TOTAL_ELAPSED ≈ 1 RTT of data (20–50 ms under 1% loss).
#
# TCP: 8 parallel goroutines, each with a full TCP+TLS connection.
#   - Each goroutine pays 2 RTTs for setup + 1 RTT data = 60 ms minimum.
#   - Any goroutine whose SYN/TLS packet is dropped stalls ≥ 200 ms.
#   - TOTAL_ELAPSED = max(individual) = often 200 ms+ under 1% loss.
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 5: Multiplex  [delay 20ms loss 1% – QUIC N independent streams vs TCP N conns]"
netem_apply "delay 20ms loss 1%"

echo "--- S5 TCP  (8 parallel connections) ---"
SSLKEYLOGFILE="$KEYLOG" "$MPLEX" -proto tcp  -n 8 -addr "$TARGET" -tcpport "$TCP_PORT"
echo ""
echo "--- S5 QUIC (8 parallel streams, 1 connection) ---"
SSLKEYLOGFILE="$KEYLOG" "$MPLEX" -proto quic -n 8 -addr "$TARGET" -quicport "$QUIC_PORT"
echo ""
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
# SCENARIO 6 – Reconnection Burst  (delay 40 ms)
# 5 short-lived connections made sequentially.  Each sends one message.
#
# TCP+TLS: every connection costs 3 RTTs (no improvement on reconnect because
#   Go's crypto/tls does NOT support TLS 1.3 0-RTT early data, and the TCP SYN
#   consumes 1 RTT before any TLS can begin).
#   Per-conn TOTAL_MS ≈ 120 ms  →  5-conn SUM ≈ 600 ms
#
# QUIC:
#   Conn 1 : DialAddr (full 1-RTT QUIC handshake)  TOTAL_MS ≈ 80 ms
#   Conn 2+: DialAddrEarly (0-RTT – data in first packet flight)
#            DialAddrEarly returns INSTANTLY; response arrives after 1 RTT.
#            TOTAL_MS ≈ 40 ms per conn
#   5-conn SUM ≈ 80 + 4×40 = 240 ms  (60% faster than TCP)
# ─────────────────────────────────────────────────────────────────────────────
hdr "SCENARIO 6: Reconnection Burst  [delay 40ms – QUIC 0-RTT vs TCP 3-RTT per conn]"
netem_apply "delay 40ms"

echo "--- S6 TCP  (5 fresh connections, session cache - no RTT saving) ---"
SSLKEYLOGFILE="$KEYLOG" "$TCP"  -burst 5 client "$TARGET" "$TCP_PORT"  1024
echo ""
echo "--- S6 QUIC (5 fresh connections, 0-RTT from conn 2 onwards) ---"
SSLKEYLOGFILE="$KEYLOG" "$QUIC" -burst 5 client "$TARGET" "$QUIC_PORT" 1024
echo ""
netem_clear

# ─────────────────────────────────────────────────────────────────────────────
hdr "Benchmark complete  $(date '+%Y-%m-%d %H:%M:%S')"
printf "  TLS keys  : %s\n" "$KEYLOG"
  printf "  macOS path: copy from node-a:/tmp/tls_keys.log (see instructions)\n"
printf "\n  Wireshark setup:\n"
printf "    Interface : vmenet0\n"
printf "    Capture   : host 192.168.2.4 and host 192.168.2.5\n"
printf "    TLS keys  : Edit→Preferences→TLS→Pre-Master-Secret log\n"
printf "    QUIC fix  : right-click gQUIC packet port 9001 → Decode As → QUIC\n\n"
