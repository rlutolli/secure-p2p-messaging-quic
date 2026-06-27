package main

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	QUICInitialStreamWindow = 6 * 1024 * 1024
	QUICInitialConnWindow   = 15 * 1024 * 1024
	QUICMaxStreamWindow     = 16 * 1024 * 1024
	QUICMaxConnWindow       = 64 * 1024 * 1024

	QUICMaxIncomingStreams    = 1000
	QUICMaxIncomingUniStreams = 1000

	QUICIdleTimeout = 60 * time.Second
	QUICKeepAlive   = 30 * time.Second

	ConnectTimeout = 5 * time.Second
	SendTimeout    = 2 * time.Second

	// MaxConcurrentHandshakes bounds how many inbound connections may be in the
	// setup phase (TLS handshake / first QUIC stream accept) at once. This
	// smooths out a thundering herd of peers connecting simultaneously so the
	// relay does not thrash on crypto or exhaust the accept queue.
	MaxConcurrentHandshakes = 256
	// HandshakeSetupTimeout caps how long the relay waits for a freshly accepted
	// connection to complete setup before dropping it, so a stalled client
	// cannot hold a setup slot indefinitely.
	HandshakeSetupTimeout = 30 * time.Second

	// OutboundQueueSize bounds the per-connection outbound message queue. Each
	// ManagedConnection is drained by a single dedicated writer goroutine, so a
	// slow or stalled peer only backs up its own queue instead of blocking the
	// relay's broadcast path (which previously ran on the per-peer reader
	// goroutine and starved PING/PONG processing, triggering false-positive
	// health-check disconnects at high peer counts). When a peer's queue is
	// full its message is dropped rather than blocking every other peer.
	OutboundQueueSize = 1024

	recommendedUDPBufSize = 7 * 1024 * 1024
)

func CheckUDPBuffers() {
	switch runtime.GOOS {
	case "linux":
		checkLinuxUDPBuffers()
	case "windows":
		log.Println("[Perf] Windows: UDP buffer size check skipped (managed by quic-go automatically)")
	}
}

func checkLinuxUDPBuffers() {
	rmem := readProcSysInt("/proc/sys/net/core/rmem_max")
	wmem := readProcSysInt("/proc/sys/net/core/wmem_max")

	if rmem == 0 && wmem == 0 {
		return
	}

	if rmem < recommendedUDPBufSize || wmem < recommendedUDPBufSize {
		log.Printf("[Perf] WARNING: Linux UDP buffers below recommended %d MB (rmem=%d wmem=%d). "+
			"Run: sudo sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608",
			recommendedUDPBufSize/1024/1024, rmem, wmem)
	} else {
		log.Printf("[Perf] Linux UDP buffers OK: rmem_max=%d, wmem_max=%d", rmem, wmem)
	}
}

func readProcSysInt(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return val
}
