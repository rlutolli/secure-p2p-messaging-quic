#!/bin/bash
INTERFACE=${1:-lo}
ACTION=${2:-apply}

if [ "$ACTION" == "apply" ]; then
    echo "[INFO] Applying 100ms latency and 20ms jitter to $INTERFACE..."
    sudo tc qdisc add dev $INTERFACE root netem delay 100ms 20ms
    echo "[SUCCESS] Network bottleneck applied."
elif [ "$ACTION" == "remove" ]; then
    echo "[INFO] Removing network bottlenecks from $INTERFACE..."
    sudo tc qdisc del dev $INTERFACE root netem 2>/dev/null
    echo "[SUCCESS] Back to normal speed."
else
    echo "Usage: ./simulate_network.sh [interface] [apply|remove]"
    echo "Example: ./simulate_network.sh lo apply"
fi
