#!/usr/bin/env bash
# =============================================================================
# vm-setup.sh  –  Provision two Multipass VMs for Wireshark traffic analysis
#
# What this script does:
#   1. Verifies Homebrew is in PATH  (you must install it first if missing)
#   2. Installs Multipass via Homebrew if not already present
#   3. Deletes any existing node-a / node-b VMs (safe re-run)
#   4. Copies project to /tmp/p2p  (macOS TCC blocks ~/Desktop from virtiofs)
#   5. Launches two Ubuntu 22.04 VMs with XFCE4 desktop + noVNC + Go,
#      mounting /tmp/p2p into each VM at /home/ubuntu/p2p
#   6. Waits for cloud-init to finish on both VMs
#   7. Builds the p2p-messenger binary inside each VM
#   8. Prints the full access guide (noVNC URLs, Wireshark settings, etc.)
#
# Usage:
#   # First time: install Homebrew (Apple Silicon path):
#   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
#   eval "$(/opt/homebrew/bin/brew shellenv)"
#
#   # Then run this script:
#   bash scripts/vm-setup.sh
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLOUD_INIT="${PROJECT_DIR}/cloud-init/common.yaml"

# ── mount source ───────────────────────────────────────────────────────────────
# macOS TCC blocks QEMU/virtiofs from accessing ~/Desktop, ~/Documents, etc.
# We copy the project to /tmp/p2p (no TCC restriction) and mount from there.
# tls_keys.log written by VMs will appear at /tmp/p2p/tls_keys.log on macOS.
MOUNT_SRC="/tmp/p2p"

# ── colour helpers ─────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${GREEN}==>${NC} $*"; }
warn() { echo -e "${YELLOW}WARN:${NC} $*"; }
die()  { echo -e "${RED}ERROR:${NC} $*" >&2; exit 1; }

# ── 1. Homebrew ────────────────────────────────────────────────────────────────
if ! command -v brew &>/dev/null; then
  # Try Apple Silicon default location
  if [[ -x /opt/homebrew/bin/brew ]]; then
    eval "$(/opt/homebrew/bin/brew shellenv)"
  elif [[ -x /usr/local/bin/brew ]]; then
    eval "$(/usr/local/bin/brew shellenv)"
  else
    die "Homebrew not found. Install it first, then re-run this script:
  /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"
  eval \"\$(/opt/homebrew/bin/brew shellenv)\""
  fi
fi
log "Homebrew: $(brew --version | head -1)"

# ── 2. Multipass ───────────────────────────────────────────────────────────────
if ! command -v multipass &>/dev/null; then
  log "Installing Multipass (this may take a minute)..."
  brew install --cask multipass
  # Give launchd a moment to register the Multipass daemon
  sleep 5
fi
log "Multipass: $(multipass --version | head -1)"

# ── 3. Tear down stale VMs ────────────────────────────────────────────────────
for VM in node-a node-b; do
  if multipass list 2>/dev/null | grep -qE "^${VM}\s"; then
    warn "Removing existing VM '${VM}'..."
    multipass stop "${VM}" 2>/dev/null || true
    multipass delete "${VM}" 2>/dev/null || true
    multipass purge 2>/dev/null || true
  fi
done

# ── 4a. Copy project to /tmp/p2p (TCC-safe mount source) ─────────────────────
# macOS denies QEMU virtiofs access to ~/Desktop and other protected folders.
# /tmp has no such restriction, so we stage a copy there first.
# The tls_keys.log written by the VMs will appear at /tmp/p2p/tls_keys.log.
log "Staging project copy at ${MOUNT_SRC} for virtiofs mount..."
rm -rf "${MOUNT_SRC}"
cp -r "${PROJECT_DIR}" "${MOUNT_SRC}"

# ── 4b. Launch VMs with native (virtiofs) mount ────────────────────────────────
# 2 vCPUs / 1.5 GB RAM / 8 GB disk per VM.
# cloud-init installs: XFCE4 desktop, x11vnc, noVNC, websockify, supervisor, Go.
# Desktop is reachable at http://<ip>:6080/vnc.html  (password: ubuntu)
#
# --mount uses native virtiofs (no snap needed; /tmp is not TCC-protected).

log "Launching node-a  (Ubuntu 22.04, XFCE4 desktop + Go)..."
multipass launch 22.04 \
  --name      node-a \
  --cpus      2      \
  --memory    1.5G   \
  --disk      8G     \
  --cloud-init "${CLOUD_INIT}" \
  --mount     "${MOUNT_SRC}:/home/ubuntu/p2p"

log "Launching node-b  (Ubuntu 22.04, XFCE4 desktop + Go)..."
multipass launch 22.04 \
  --name      node-b \
  --cpus      2      \
  --memory    1.5G   \
  --disk      8G     \
  --cloud-init "${CLOUD_INIT}" \
  --mount     "${MOUNT_SRC}:/home/ubuntu/p2p"

# ── 5. Wait for cloud-init ────────────────────────────────────────────────────
# cloud-init installs packages + Go – this typically takes 3-8 minutes.
log "Waiting for cloud-init on node-a  (installs packages + Go – may take ~5 min)..."
multipass exec node-a -- cloud-init status --wait --long

log "Waiting for cloud-init on node-b..."
multipass exec node-b -- cloud-init status --wait --long

# ── 6. Build the binary inside each VM ───────────────────────────────────────
# Binary goes to /home/ubuntu/p2p-messenger (NOT inside the shared mount)
# so it doesn't overwrite the macOS binary.
BUILD_CMD='PATH=$PATH:/usr/local/go/bin && cd /home/ubuntu/p2p && go build -o /home/ubuntu/p2p-messenger . && echo "Build OK: $(go version)"'

log "Building p2p-messenger inside node-a..."
multipass exec node-a -- /bin/bash -c "${BUILD_CMD}"

log "Building p2p-messenger inside node-b..."
multipass exec node-b -- /bin/bash -c "${BUILD_CMD}"

# ── 7. Collect IPs ────────────────────────────────────────────────────────────
NODE_A_IP=$(multipass info node-a | awk '/IPv4/{print $2; exit}')
NODE_B_IP=$(multipass info node-b | awk '/IPv4/{print $2; exit}')

# ── 8. Print usage guide ──────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}┌──────────────────────────────────────────────────────────────────┐${NC}"
echo -e "${GREEN}│              VMs ready – here is your access guide               │${NC}"
echo -e "${GREEN}├──────────────────────────────────────────────────────────────────┤${NC}"
echo -e "${GREEN}│  VM DESKTOP (open in browser – no VNC app needed)                │${NC}"
echo    "│                                                                  │"
echo    "│   node-a:  http://${NODE_A_IP}:6080/vnc.html"
echo    "│   node-b:  http://${NODE_B_IP}:6080/vnc.html"
echo    "│   Password: ubuntu                                               │"
echo    "│                                                                  │"
echo -e "${GREEN}│  CLI SHELL (opens an interactive terminal inside the VM)          │${NC}"
echo    "│                                                                  │"
echo    "│   multipass shell node-a                                         │"
echo    "│   multipass shell node-b                                         │"
echo    "│                                                                  │"
echo -e "${GREEN}│  RUN THE MESSENGER (on each VM)                                   │${NC}"
echo    "│                                                                  │"
echo    "│   SSLKEYLOGFILE=/home/ubuntu/p2p/tls_keys.log \\                │"
echo    "│   /home/ubuntu/p2p-messenger                                     │"
echo    "│                                                                  │"
echo    "│   Tip: use  --use-tcp  flag to switch to TCP+TLS mode            │"
echo    "│                                                                  │"
echo -e "${GREEN}│  WIRESHARK (run on this macOS host)                               │${NC}"
echo    "│                                                                  │"
echo    "│   Capture interface : bridge100                                  │"
echo    "│   TLS key log file  : ${MOUNT_SRC}/tls_keys.log"
echo    "│     (Edit > Preferences > Protocols > TLS > Pre-Master-Secret)   │"
echo    "│                                                                  │"
echo    "│   Useful display filters:                                        │"
echo    "│     quic              → all QUIC (UDP) traffic                   │"
echo    "│     tcp               → TCP+TLS traffic  (use --use-tcp flag)    │"
echo    "│     udp.port == 19999 → peer discovery JSON broadcasts           │"
echo    "│                                                                  │"
echo -e "${GREEN}│  TEARDOWN                                                         │${NC}"
echo    "│                                                                  │"
echo    "│   multipass stop  node-a node-b                                  │"
echo    "│   multipass delete node-a node-b && multipass purge              │"
echo -e "${GREEN}└──────────────────────────────────────────────────────────────────┘${NC}"
echo ""
