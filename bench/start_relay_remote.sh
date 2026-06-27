#!/bin/bash
# start_relay_remote.sh — start the patched p2p relay detached on the EU host.
# This replicates the known-good launch: background `tail -f FIFO | relay` so
# the relay's stdin stays open with the room name, then let this script exit.
# Invoked via `setsid bash start_relay_remote.sh` so the pipeline is orphaned
# into its own session and survives SSH disconnect.
cd ~/secure-p2p-messaging-quic
pkill -f "p2p-messenger" 2>/dev/null
pkill -f "tail -f /tmp/relay_in" 2>/dev/null
sleep 2
: > relay.log
printf "benchmark\n" > /tmp/relay_in
tail -f /tmp/relay_in | ./p2p-messenger --relay --no-upnp --disable-gso --disable-ecn --relay-port 41748 > relay.log 2>&1 &
echo $! > /tmp/relay.pid
sleep 5
echo "PID=$(cat /tmp/relay.pid)"
