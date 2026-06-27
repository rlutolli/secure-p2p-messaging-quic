// multiplex_bench.go – Multiplexed streams: QUIC N independent streams vs TCP N connections
//
// Usage (client-only tool – connects to existing tcp_server / quic_server):
//
//	go run multiplex_bench.go -proto quic -n 8 [-size 1024] [-addr 192.168.2.5]
//	go run multiplex_bench.go -proto tcp  -n 8 [-size 1024] [-addr 192.168.2.5]
//
// QUIC mode: ONE connection (0-RTT after warmup), N independent streams opened
//
//	simultaneously.  Packet loss on one stream does NOT stall the others.
//
// TCP mode:  N parallel goroutines each opening a fresh TCP+TLS connection.
//
//	Each connection costs 2 RTTs (TCP SYN + TLS HS) before data can flow.
//	Under packet loss, any dropped SYN or handshake packet stalls that goroutine
//	for ≥ 200 ms (Linux TCP RTO minimum).
//
// Set SSLKEYLOGFILE=... so Wireshark can decrypt the traffic.
package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

var (
	flagProto    = flag.String("proto", "quic", "protocol: tcp or quic")
	flagN        = flag.Int("n", 8, "number of parallel goroutines/streams")
	flagSize     = flag.Int("size", 1024, "payload size in bytes")
	flagAddr     = flag.String("addr", "192.168.2.5", "server address")
	flagTCPPort  = flag.String("tcpport", "9000", "TCP server port")
	flagQUICPort = flag.String("quicport", "9001", "QUIC server port")
	flagCSV      = flag.Bool("csv", false, "also emit one machine-readable row: 'CSV,proto,streams,size,min_ms,p50_ms,max_ms,avg_ms,total_elapsed_ms'")
)

// ─── Shared helpers ──────────────────────────────────────────────────────────

func openKeyLog() io.WriteCloser {
	path := os.Getenv("SSLKEYLOGFILE")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Printf("[WARN] key log open failed: %v\n", err)
		return nil
	}
	return f
}

func writeMsg(w io.Writer, msg []byte) error {
	buf := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	return err
}

func readMsg(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	sz := binary.BigEndian.Uint32(hdr)
	buf := make([]byte, sz)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func makePayload(idx, size int) []byte {
	hdr := fmt.Sprintf("MPLEX%d:SIZE=%d:", idx, size)
	if len(hdr) >= size {
		return []byte(hdr[:size])
	}
	return []byte(hdr + strings.Repeat("A", size-len(hdr)))
}

func main() {
	flag.Parse()
	kl := openKeyLog()
	if kl != nil {
		defer kl.Close()
	}

	switch *flagProto {
	case "quic":
		runQUICMultiplex(*flagAddr, *flagQUICPort, *flagN, *flagSize, kl)
	case "tcp":
		runTCPMultiplex(*flagAddr, *flagTCPPort, *flagN, *flagSize, kl)
	default:
		fmt.Fprintf(os.Stderr, "Unknown proto %q — use tcp or quic\n", *flagProto)
		os.Exit(1)
	}
}

// ─── QUIC multiplex ──────────────────────────────────────────────────────────
// Phase 1 – Warmup: open a 1-RTT connection so the server issues a session
//   ticket.  Sleep briefly to ensure the ticket arrives before closing.
// Phase 2 – Benchmark: DialAddrEarly (0-RTT, returns instantly).  Open N
//   streams concurrently.  Each stream sends one message and reads the echo.
//   Loss on stream i is isolated — streams j≠i continue unaffected.

func runQUICMultiplex(addr, port string, n, size int, kl io.Writer) {
	ctx := context.Background()
	sessionCache := tls.NewLRUClientSessionCache(100)

	baseTLS := func() *tls.Config {
		return &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-diagnostic"},
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       kl,
			ClientSessionCache: sessionCache,
		}
	}

	quicCfg := &quic.Config{
		MaxIdleTimeout:                 2 * time.Minute,
		KeepAlivePeriod:                30 * time.Second,
		MaxIncomingStreams:             1000,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
		Allow0RTT:                      true,
	}

	target := net.JoinHostPort(addr, port)

	// ── Phase 1: Warmup ────────────────────────────────────────────────────
	fmt.Printf("[QUIC MULTIPLEX n=%d size=%d] warming up session cache…\n", n, size)
	wConn, err := quic.DialAddr(ctx, target, baseTLS(), quicCfg)
	if err != nil {
		fmt.Printf("Warmup dial error: %v\n", err)
		return
	}
	// Send a dummy message so the server keeps the connection alive long enough
	// to issue a session ticket (sent ~0 ms after handshake, arrives after 1 RTT).
	ws, _ := wConn.OpenStreamSync(ctx)
	writeMsg(ws, makePayload(0, 64))
	readMsg(ws)
	ws.Close()
	// Wait for session ticket to arrive (1 RTT + margin).
	time.Sleep(300 * time.Millisecond)
	wConn.CloseWithError(0, "warmup done")
	fmt.Printf("  session ticket cached — starting 0-RTT benchmark\n\n")

	// ── Phase 2: Benchmark ─────────────────────────────────────────────────
	// Use DialAddr (1-RTT handshake) — the Scenario 5 win comes from N streams
	// on ONE connection vs TCP's N separate connections (each costing 2 RTTs).
	// 0-RTT is demonstrated in Scenario 6; here we focus on multiplexing.
	overallStart := time.Now()
	conn, err := quic.DialAddr(ctx, target, baseTLS(), quicCfg)
	if err != nil {
		fmt.Printf("DialAddr error: %v\n", err)
		return
	}
	dialMs := float64(time.Since(overallStart).Microseconds()) / 1000.0

	results := make([]time.Duration, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			stream, err := conn.OpenStreamSync(ctx)
			if err != nil {
				fmt.Printf("  [stream %d] OpenStream error: %v\n", idx+1, err)
				return
			}
			defer stream.Close()

			payload := makePayload(idx+1, size)
			start := time.Now()
			if err := writeMsg(stream, payload); err != nil {
				fmt.Printf("  [stream %d] Write error: %v\n", idx+1, err)
				return
			}
			if _, err := readMsg(stream); err != nil {
				fmt.Printf("  [stream %d] Read error: %v\n", idx+1, err)
				return
			}
			results[idx] = time.Since(start)
		}(i)
	}
	wg.Wait()
	totalElapsed := time.Since(overallStart)
	conn.CloseWithError(0, "done")

	printResults("QUIC", n, size, dialMs, results, totalElapsed)
}

// ─── TCP multiplex ───────────────────────────────────────────────────────────
// N goroutines each open a fresh TCP+TLS connection.
// Phase 1 – Warmup: one connection to prime the TLS session cache (PSK).
//   NOTE: Even with session resumption, TCP+TLS cannot avoid the TCP SYN RTT.
//   Every connection still costs: TCP SYN (1 RTT) + TLS HS (1 RTT) + data (1 RTT).
// Phase 2 – Benchmark: N goroutines launched simultaneously.
//   Total elapsed = max(individual connection times).
//   Under loss, any goroutine whose SYN or TLS packet is dropped stalls for
//   ≥ 200 ms (Linux tcp_rto_min), pulling the max — and thus the total — upward.

func runTCPMultiplex(addr, port string, n, size int, kl io.Writer) {
	target := net.JoinHostPort(addr, port)
	sessionCache := tls.NewLRUClientSessionCache(100)

	baseTLS := func() *tls.Config {
		return &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       kl,
			ClientSessionCache: sessionCache,
		}
	}

	// ── Phase 1: Warmup ────────────────────────────────────────────────────
	fmt.Printf("[TCP MULTIPLEX n=%d size=%d] warming up TLS session cache…\n", n, size)
	wConn, err := tls.Dial("tcp", target, baseTLS())
	if err != nil {
		fmt.Printf("Warmup dial error: %v\n", err)
		return
	}
	writeMsg(wConn, makePayload(0, 64))
	readMsg(wConn)
	wConn.Close()
	// TLS session ticket arrives in a separate post-handshake message; give it time.
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("  TLS session cached — starting parallel benchmark\n\n")

	// ── Phase 2: Benchmark ─────────────────────────────────────────────────
	results := make([]time.Duration, n)
	var wg sync.WaitGroup

	overallStart := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := time.Now()
			conn, err := tls.Dial("tcp", target, baseTLS())
			if err != nil {
				fmt.Printf("  [conn %d] Dial error: %v\n", idx+1, err)
				return
			}
			defer conn.Close()

			payload := makePayload(idx+1, size)
			if err := writeMsg(conn, payload); err != nil {
				fmt.Printf("  [conn %d] Write error: %v\n", idx+1, err)
				return
			}
			if _, err := readMsg(conn); err != nil {
				fmt.Printf("  [conn %d] Read error: %v\n", idx+1, err)
				return
			}
			results[idx] = time.Since(start)
		}(i)
	}
	wg.Wait()
	totalElapsed := time.Since(overallStart)

	printResults("TCP", n, size, -1, results, totalElapsed)
}

// ─── Result printer ──────────────────────────────────────────────────────────

func printResults(proto string, n, size int, dialMs float64, results []time.Duration, total time.Duration) {
	if dialMs >= 0 {
		fmt.Printf("[%s MULTIPLEX n=%d size=%d]  DIAL_MS=%.1f (0-RTT, before streams opened)\n",
			proto, n, size, dialMs)
	} else {
		fmt.Printf("[%s MULTIPLEX n=%d size=%d]\n", proto, n, size)
	}

	var sum time.Duration
	var maxD, minD time.Duration
	valid := 0
	for i, d := range results {
		if d == 0 {
			fmt.Printf("  [%d] ERROR (see above)\n", i+1)
			continue
		}
		fmt.Printf("  [%d] RTT=%v\n", i+1, d)
		sum += d
		if maxD == 0 || d > maxD {
			maxD = d
		}
		if minD == 0 || d < minD {
			minD = d
		}
		valid++
	}

	// Compute p50 (median)
	sorted := make([]time.Duration, 0, valid)
	for _, d := range results {
		if d > 0 {
			sorted = append(sorted, d)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var p50 time.Duration
	if len(sorted) > 0 {
		p50 = sorted[len(sorted)/2]
	}

	if valid > 0 {
		fmt.Printf("  ── min=%v  p50=%v  max=%v  avg=%v\n",
			minD, p50, maxD, sum/time.Duration(valid))
	}
	fmt.Printf("  ── TOTAL_ELAPSED=%v  (wall-clock from first goroutine launch to last response)\n",
		total)

	if *flagCSV && valid > 0 {
		msf := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
		// Machine-readable row consumed by bench/multistream_study.sh. The "CSV,"
		// prefix lets the orchestrator grep it out of the human-readable output.
		fmt.Printf("CSV,%s,%d,%d,%.3f,%.3f,%.3f,%.3f,%.3f\n",
			strings.ToLower(proto), n, size,
			msf(minD), msf(p50), msf(maxD), msf(sum/time.Duration(valid)), msf(total))
	}
}
