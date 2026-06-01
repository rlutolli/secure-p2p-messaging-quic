package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// Protocol replicated from the main p2p-messenger app (cannot be imported).

// safeWriter serialises writes to a shared connection so that the main
// message-sending goroutine and the PONG-reply goroutine do not race.
type safeWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func (sw *safeWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// quicConn wraps a QUIC connection + stream so it satisfies io.ReadWriteCloser.
type quicConn struct {
	stream *quic.Stream
	qconn  *quic.Conn
}

func (c *quicConn) Read(p []byte) (n int, err error)  { return c.stream.Read(p) }
func (c *quicConn) Write(p []byte) (n int, err error) { return c.stream.Write(p) }
func (c *quicConn) Close() error {
	c.stream.Close()
	c.qconn.CloseWithError(0, "loadtest done")
	return nil
}

// globalTracker stores send times keyed by message ID shared across all peers.
type globalTracker struct {
	mu        sync.Mutex
	sendTimes map[string]time.Time
}

// peerResult holds per-peer measurements.
type peerResult struct {
	peerID       int
	dialMs       float64
	joinMs       float64
	msgsSent     atomic.Int64
	msgsRecv     atomic.Int64
	rtts         []float64
	rttMu        sync.Mutex
	errors       atomic.Int64
	disconnected atomic.Int32
}

func main() {
	var cfg struct {
		n           int
		target      string
		room        string
		password    string
		rate        int
		duration    int
		proto       string
		size        int
		aliasPrefix string
		output      string
		csv         bool
	}

	flag.IntVar(&cfg.n, "n", 50, "number of concurrent peers")
	flag.StringVar(&cfg.target, "target", "127.0.0.1:0", "target address:port")
	flag.StringVar(&cfg.room, "room", "bench-room", "room name to join")
	flag.StringVar(&cfg.password, "password", "", "room password")
	flag.IntVar(&cfg.rate, "rate", 1, "messages per second per peer")
	flag.IntVar(&cfg.duration, "duration", 30, "test duration in seconds")
	flag.StringVar(&cfg.proto, "proto", "quic", "protocol: quic or tcp")
	flag.IntVar(&cfg.size, "size", 100, "message payload size in bytes")
	flag.StringVar(&cfg.aliasPrefix, "alias-prefix", "peer", "prefix for peer aliases")
	flag.StringVar(&cfg.output, "output", "", "output file for CSV (default: stdout)")
	flag.BoolVar(&cfg.csv, "csv", true, "emit results in CSV format")
	flag.Parse()

	if cfg.target == "" || cfg.target == "127.0.0.1:0" {
		fmt.Fprintln(os.Stderr, "Error: -target is required (e.g. 127.0.0.1:8080)")
		flag.Usage()
		os.Exit(1)
	}
	if cfg.proto != "quic" && cfg.proto != "tcp" {
		fmt.Fprintf(os.Stderr, "Error: -proto must be 'quic' or 'tcp', got %q\n", cfg.proto)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make([]peerResult, cfg.n)
	var wg sync.WaitGroup

	gt := &globalTracker{sendTimes: make(map[string]time.Time)}
	for i := 0; i < cfg.n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			results[id] = runPeer(ctx, cfg, id, gt)
		}(i)
	}

	wg.Wait()

	aggregate := aggregateResults(results, cfg)

	if cfg.csv {
		out := os.Stdout
		if cfg.output != "" {
			f, err := os.Create(cfg.output)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
				os.Exit(1)
			}
			defer f.Close()
			out = f
		}
		if err := writeCSV(out, aggregate); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing CSV: %v\n", err)
			os.Exit(1)
		}
	} else {
		printSummary(aggregate)
	}
}

func runPeer(ctx context.Context, cfg struct {
	n           int
	target      string
	room        string
	password    string
	rate        int
	duration    int
	proto       string
	size        int
	aliasPrefix string
	output      string
	csv         bool
}, peerID int, gt *globalTracker) peerResult {
	res := peerResult{peerID: peerID}
	alias := fmt.Sprintf("%s-%d", cfg.aliasPrefix, peerID)

	// 1. Dial
	dialStart := time.Now()
	var conn io.ReadWriteCloser
	var err error

	if cfg.proto == "quic" {
		conn, err = dialQUIC(ctx, cfg.target)
	} else {
		conn, err = dialTCP(ctx, cfg.target)
	}
	if err != nil {
		res.errors.Add(1)
		return res
	}
	defer conn.Close()
	res.dialMs = float64(time.Since(dialStart).Milliseconds())

	sw := &safeWriter{w: conn}
	readerDone := make(chan struct{})
	var senderFinished int32

	// 2. Start reader goroutine (PING/PONG + RTT tracking)
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					res.errors.Add(1)
				}
				if atomic.LoadInt32(&senderFinished) == 0 {
					res.disconnected.Store(1)
				}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "PING" {
				if _, werr := sw.Write([]byte("PONG\n")); werr != nil {
					res.errors.Add(1)
				}
				continue
			}
			if strings.HasPrefix(line, "AUTH:FAILED") {
				res.errors.Add(1)
				continue
			}
			if strings.HasPrefix(line, "FROM:") {
				parts := strings.SplitN(strings.TrimPrefix(line, "FROM:"), "|", 3)
				if len(parts) >= 2 {
					msgID := parts[1]
					gt.mu.Lock()
					if sendTime, ok := gt.sendTimes[msgID]; ok {
						rtt := float64(time.Since(sendTime).Milliseconds())
						res.rttMu.Lock()
						res.rtts = append(res.rtts, rtt)
						res.rttMu.Unlock()
						res.msgsRecv.Add(1)
					}
					gt.mu.Unlock()
				}
			}
			// SYSTEM: and other lines are ignored for RTT purposes.
		}
	}()

	// 3. Join room
	var joinMsg string
	if cfg.password != "" {
		joinMsg = fmt.Sprintf("JOIN:%s|%s|%s\n", cfg.room, alias, cfg.password)
	} else {
		joinMsg = fmt.Sprintf("JOIN:%s|%s\n", cfg.room, alias)
	}

	joinStart := time.Now()
	if _, err := sw.Write([]byte(joinMsg)); err != nil {
		res.errors.Add(1)
		return res
	}
	// The server does not send a direct JOIN confirmation; we consider join
	// complete once the handshake bytes have been written.
	res.joinMs = float64(time.Since(joinStart).Milliseconds())

	// 4. Message loop
	ticker := time.NewTicker(time.Second / time.Duration(cfg.rate))
	defer ticker.Stop()

	deadline := time.Now().Add(time.Duration(cfg.duration) * time.Second)
	msgIndex := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			goto done
		case <-ticker.C:
		}

		msgID := fmt.Sprintf("%s-%d", alias, msgIndex)
		msgIndex++

		payload := buildPayload(msgID, cfg.size)
		line := fmt.Sprintf("FROM:%s|%s\n", alias, payload)

		gt.mu.Lock()
		gt.sendTimes[msgID] = time.Now()
		gt.mu.Unlock()

		if _, err := sw.Write([]byte(line)); err != nil {
			res.errors.Add(1)
			break
		}
		res.msgsSent.Add(1)
	}

done:
	atomic.StoreInt32(&senderFinished, 1)
	// Allow a brief moment for in-flight replies to arrive before closing.
	time.Sleep(200 * time.Millisecond)
	conn.Close()
	<-readerDone
	return res
}

func dialQUIC(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
		MinVersion:         tls.VersionTLS13,
	}
	qconn, err := quic.DialAddr(ctx, target, tlsConf, &quic.Config{})
	if err != nil {
		return nil, fmt.Errorf("dialQUIC: %w", err)
	}
	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		qconn.CloseWithError(1, "stream open failed")
		return nil, fmt.Errorf("dialQUIC stream open: %w", err)
	}
	return &quicConn{stream: stream, qconn: qconn}, nil
}

func dialTCP(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("dialTCP: %w", err)
	}
	tlsConn := tls.Client(rawConn, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
		MinVersion:         tls.VersionTLS13,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("dialTCP handshake: %w", err)
	}
	return tlsConn, nil
}

func buildPayload(msgID string, size int) string {
	prefix := msgID + "|"
	if len(prefix) >= size {
		if size <= 0 {
			return ""
		}
		if size > len(msgID) {
			return msgID
		}
		return msgID[:size]
	}
	padding := size - len(prefix)
	return prefix + strings.Repeat("A", padding)
}

// aggregateResults computes the final summary statistics.
type aggregateResult struct {
	timestamp               string
	proto                   string
	n                       int
	rate                    int
	duration                int
	size                    int
	target                  string
	dialMsAvg               float64
	dialMsP95               float64
	joinMsAvg               float64
	joinMsP95               float64
	totalMsgsSent           int64
	totalMsgsRecv           int64
	avgThroughputMsgsPerSec float64
	avgThroughputKBPerSec   float64
	minRttMs                float64
	p50RttMs                float64
	p95RttMs                float64
	p99RttMs                float64
	maxRttMs                float64
	errorsTotal             int64
	disconnectsTotal        int64
}

func aggregateResults(results []peerResult, cfg struct {
	n           int
	target      string
	room        string
	password    string
	rate        int
	duration    int
	proto       string
	size        int
	aliasPrefix string
	output      string
	csv         bool
}) aggregateResult {
	var totalMsgsSent, totalMsgsRecv, errorsTotal, disconnectsTotal int64
	var allDialMs, allJoinMs, allRtts []float64

	for i := range results {
		r := &results[i]
		totalMsgsSent += r.msgsSent.Load()
		totalMsgsRecv += r.msgsRecv.Load()
		errorsTotal += r.errors.Load()
		disconnectsTotal += int64(r.disconnected.Load())
		allDialMs = append(allDialMs, r.dialMs)
		allJoinMs = append(allJoinMs, r.joinMs)
		r.rttMu.Lock()
		allRtts = append(allRtts, r.rtts...)
		r.rttMu.Unlock()
	}

	agg := aggregateResult{
		timestamp:        time.Now().Format(time.RFC3339),
		proto:            cfg.proto,
		n:                cfg.n,
		rate:             cfg.rate,
		duration:         cfg.duration,
		size:             cfg.size,
		target:           cfg.target,
		totalMsgsSent:    totalMsgsSent,
		totalMsgsRecv:    totalMsgsRecv,
		errorsTotal:      errorsTotal,
		disconnectsTotal: disconnectsTotal,
	}

	agg.dialMsAvg, agg.dialMsP95 = stats(allDialMs)
	agg.joinMsAvg, agg.joinMsP95 = stats(allJoinMs)

	if len(allRtts) > 0 {
		sort.Float64s(allRtts)
		agg.minRttMs = allRtts[0]
		agg.p50RttMs = percentile(allRtts, 50)
		agg.p95RttMs = percentile(allRtts, 95)
		agg.p99RttMs = percentile(allRtts, 99)
		agg.maxRttMs = allRtts[len(allRtts)-1]
	}

	agg.avgThroughputMsgsPerSec = float64(totalMsgsSent) / float64(cfg.n) / float64(cfg.duration)
	agg.avgThroughputKBPerSec = float64(totalMsgsSent*int64(cfg.size)) / 1024.0 / float64(cfg.n) / float64(cfg.duration)

	return agg
}

func stats(values []float64) (avg, p95 float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Float64s(values)
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	avg = sum / float64(len(values))
	p95 = percentile(values, 95)
	return
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p/100)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func writeCSV(w io.Writer, agg aggregateResult) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	headers := []string{
		"timestamp", "proto", "n", "rate", "duration", "size", "target",
		"dial_ms_avg", "dial_ms_p95", "join_ms_avg", "join_ms_p95",
		"total_msgs_sent", "total_msgs_recv",
		"avg_throughput_msgs_per_sec", "avg_throughput_kb_per_sec",
		"min_rtt_ms", "p50_rtt_ms", "p95_rtt_ms", "p99_rtt_ms", "max_rtt_ms",
		"errors_total", "disconnects_total",
	}
	if err := cw.Write(headers); err != nil {
		return fmt.Errorf("writeCSV headers: %w", err)
	}

	record := []string{
		agg.timestamp,
		agg.proto,
		fmt.Sprintf("%d", agg.n),
		fmt.Sprintf("%d", agg.rate),
		fmt.Sprintf("%d", agg.duration),
		fmt.Sprintf("%d", agg.size),
		agg.target,
		fmt.Sprintf("%.3f", agg.dialMsAvg),
		fmt.Sprintf("%.3f", agg.dialMsP95),
		fmt.Sprintf("%.3f", agg.joinMsAvg),
		fmt.Sprintf("%.3f", agg.joinMsP95),
		fmt.Sprintf("%d", agg.totalMsgsSent),
		fmt.Sprintf("%d", agg.totalMsgsRecv),
		fmt.Sprintf("%.3f", agg.avgThroughputMsgsPerSec),
		fmt.Sprintf("%.3f", agg.avgThroughputKBPerSec),
		fmt.Sprintf("%.3f", agg.minRttMs),
		fmt.Sprintf("%.3f", agg.p50RttMs),
		fmt.Sprintf("%.3f", agg.p95RttMs),
		fmt.Sprintf("%.3f", agg.p99RttMs),
		fmt.Sprintf("%.3f", agg.maxRttMs),
		fmt.Sprintf("%d", agg.errorsTotal),
		fmt.Sprintf("%d", agg.disconnectsTotal),
	}
	if err := cw.Write(record); err != nil {
		return fmt.Errorf("writeCSV record: %w", err)
	}
	return nil
}

func printSummary(agg aggregateResult) {
	fmt.Printf("Load test complete\n")
	fmt.Printf("  Target:      %s\n", agg.target)
	fmt.Printf("  Protocol:    %s\n", agg.proto)
	fmt.Printf("  Peers:       %d\n", agg.n)
	fmt.Printf("  Duration:    %ds\n", agg.duration)
	fmt.Printf("  Rate/peer:   %d msg/s\n", agg.rate)
	fmt.Printf("  Payload:     %d bytes\n", agg.size)
	fmt.Printf("  Dial avg:    %.3f ms (p95 %.3f ms)\n", agg.dialMsAvg, agg.dialMsP95)
	fmt.Printf("  Join avg:    %.3f ms (p95 %.3f ms)\n", agg.joinMsAvg, agg.joinMsP95)
	fmt.Printf("  Msgs sent:   %d\n", agg.totalMsgsSent)
	fmt.Printf("  Msgs recv:   %d\n", agg.totalMsgsRecv)
	fmt.Printf("  Throughput:  %.3f msg/s/peer (%.3f KB/s/peer)\n", agg.avgThroughputMsgsPerSec, agg.avgThroughputKBPerSec)
	fmt.Printf("  RTT:         min %.3f / p50 %.3f / p95 %.3f / p99 %.3f / max %.3f ms\n",
		agg.minRttMs, agg.p50RttMs, agg.p95RttMs, agg.p99RttMs, agg.maxRttMs)
	fmt.Printf("  Errors:      %d\n", agg.errorsTotal)
	fmt.Printf("  Disconnects: %d\n", agg.disconnectsTotal)
}
