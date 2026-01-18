#!/bin/bash
# AI GENERATED FOR OPTIMIZATION
#
# apply_sysctl.sh - Apply optimal kernel settings for QUIC performance
#
# This script configures Linux kernel parameters for:
# - Maximum UDP buffer sizes (128MB)
# - Network backlog optimization
#
# NOTE: busy_poll is EXCLUDED as it hurts user-space QUIC performance
#
# Usage:
#   sudo ./scripts/apply_sysctl.sh

set -e

echo "=== QUIC Performance Tuning Script ==="
echo ""

# ============================================================================
# UDP BUFFER SIZES (KEEP - helps QUIC)
# ============================================================================
echo "[1/3] Setting UDP buffer sizes..."
sysctl -w net.core.rmem_max=26214400    # 25 MB max
sysctl -w net.core.wmem_max=26214400
sysctl -w net.core.rmem_default=212992
sysctl -w net.core.wmem_default=212992

# ============================================================================
# NETWORK BACKLOG (KEEP - helps all networking)
# ============================================================================
echo "[2/3] Increasing network backlog..."
sysctl -w net.core.netdev_max_backlog=5000
sysctl -w net.core.somaxconn=4096

# ============================================================================
# UDP MEMORY LIMITS (KEEP - helps QUIC)
# ============================================================================
echo "[3/3] Setting UDP memory limits..."
sysctl -w net.ipv4.udp_mem="10240 87380 16777216"
sysctl -w net.ipv4.udp_rmem_min=16384
sysctl -w net.ipv4.udp_wmem_min=16384

# ============================================================================
# NOT APPLIED - busy_poll hurts user-space QUIC
# ============================================================================
# net.core.busy_poll=0    # DO NOT enable for QUIC
# net.core.busy_read=0    # DO NOT enable for QUIC

echo ""
echo "=== Done ==="
echo "Note: busy_poll is NOT enabled as it hurts user-space QUIC"
