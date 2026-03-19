#!/bin/bash
# tune_udp.sh - Increase Linux UDP buffer sizes for high-performance QUIC

echo "Trying to increase UDP buffer limits..."
# Try adjusting buffers; ignore errors if keys don't exist (some containers lack access)
sysctl -w net.core.rmem_max=8388608 2>/dev/null || true
sysctl -w net.core.wmem_max=8388608 2>/dev/null || true

echo "Current limits:"
sysctl net.core.rmem_max net.core.wmem_max 2>/dev/null || echo "Unable to read current limits."
