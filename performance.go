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

	recommendedUDPBufSize = 7 * 1024 * 1024
)

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

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
