#!/bin/bash

INTERFACE="eth0"
ACTION=$1
LATENCY=${2:-50}
JITTER=${3:-5}

if [ "$ACTION" == "apply" ]; then
    echo "Applying WAN simulation: ${LATENCY}ms latency, ${JITTER}ms jitter on ${INTERFACE}..."
    sudo tc qdisc add dev $INTERFACE root netem delay ${LATENCY}ms ${JITTER}ms
elif [ "$ACTION" == "clear" ]; then
    echo "Clearing WAN simulation on ${INTERFACE}..."
    sudo tc qdisc del dev $INTERFACE root netem 2>/dev/null
else
    echo "Usage: $0 [apply|clear] [latency_ms] [jitter_ms]"
fi
