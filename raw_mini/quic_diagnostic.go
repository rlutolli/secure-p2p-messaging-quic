// quic_diagnostic.go – QUIC+TLS echo benchmark with SSL key logging for Wireshark
//
// Usage:
//
//		go run quic_diagnostic.go [-opt] [-reuse] [-wan] [-burst N] server [port]
//		go run quic_diagnostic.go [-opt] [-reuse] [-wan] [-burst N] client [addr] [port] [size]
//
//		-opt      Enable production QUIC config (large windows, Allow0RTT) + TLS session cache
//	  -reuse    Run all payload sizes (64, 100, 5000 B) on a single persistent connection
//		-wan      Include handshake in timing; timer starts before quic.DialAddr (Scenario 2)
//		-burst N  Make N short-lived fresh connections sequentially; 2nd+ use DialAddrEarly / 0-RTT (Scenario 6)
//
// Payload format:
//
//	"MSG1:QUIC:SIZE=64:AAAA..."  padded with 'A' to exactly <size> bytes
//
// Set SSLKEYLOGFILE=/path/to/tls_keys.log so Wireshark can decrypt traffic.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	DefaultPort    = "9001"
	KeyLogFallback = "tls_keys.txt"
)

var (
	flagOpt   = flag.Bool("opt", false, "enable production QUIC config + TLS session cache / 0-RTT")
	flagReuse = flag.Bool("reuse", false, "run all sizes (64,100,5000) on one persistent connection")
	flagWAN   = flag.Bool("wan", false, "include handshake in timing; timer starts before quic.DialAddr")
	flagBurst = flag.Int("burst", 0, "make N short-lived connections; 2nd+ use DialAddrEarly (0-RTT)")
)

var sweepSizes = []int{64, 100, 5000}

func openKeyLog() io.WriteCloser {
	path := os.Getenv("SSLKEYLOGFILE")
	if path == "" {
		path = KeyLogFallback
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Printf("[WARN] Could not open key log file %q: %v\n", path, err)
		return nil
	}
	fmt.Printf("[TLS] Key log → %s\n", path)
	return f
}

func makePayload(msgNum, size int) string {
	header := fmt.Sprintf("MSG%d:QUIC:SIZE=%d:", msgNum, size)
	if len(header) >= size {
		return header[:size]
	}
	return header + strings.Repeat("A", size-len(header))
}

// writeMsg sends a 4-byte big-endian length prefix + body in a SINGLE Write call.
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
	size := binary.BigEndian.Uint32(hdr)
	buf := make([]byte, size)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

// generateTLSConfig builds an ephemeral self-signed TLS 1.3 config for QUIC.
func generateTLSConfig(keyLog io.Writer) *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"QUIC Benchmark"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("192.168.2.2"),
			net.ParseIP("192.168.2.3"),
		},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"quic-diagnostic"},
		MinVersion:   tls.VersionTLS13,
		KeyLogWriter: keyLog,
	}
}

// productionQuicConfig returns a tuned QUIC config: large receive windows to
// eliminate flow-control stalls, and Allow0RTT to accept 0-RTT data from clients
// that have a cached session ticket.
func productionQuicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 5 * time.Minute,
		KeepAlivePeriod:                30 * time.Second,
		MaxIncomingStreams:             1000,
		MaxIncomingUniStreams:          1000,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
		Allow0RTT:                      true,
		DisablePathMTUDiscovery:        false,
	}
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: quic_diagnostic [-opt] [-reuse] [-wan] [-burst N] server [port]")
		fmt.Println("       quic_diagnostic [-opt] [-reuse] [-wan] [-burst N] client [addr] [port] [size]")
		os.Exit(1)
	}

	switch args[0] {
	case "server":
		port := DefaultPort
		if len(args) >= 2 {
			port = args[1]
		}
		runServer(port)

	case "client":
		addr := "127.0.0.1"
		port := DefaultPort
		size := 64
		if len(args) >= 2 {
			addr = args[1]
		}
		if len(args) >= 3 {
			port = args[2]
		}
		if len(args) >= 4 {
			n, err := strconv.Atoi(args[3])
			if err != nil || n < 1 {
				fmt.Printf("Invalid size %q\n", args[3])
				os.Exit(1)
			}
			size = n
		}

		switch {
		case *flagBurst > 0:
			runClientBurst(addr, port, size, *flagBurst)
		case *flagWAN:
			runClientWAN(addr, port, size)
		case *flagReuse:
			runClientReuse(addr, port, *flagOpt)
		case *flagOpt:
			runClientOpt(addr, port)
		default:
			runClient(addr, port, size)
		}

	default:
		fmt.Printf("Unknown mode: %s\n", args[0])
		os.Exit(1)
	}
}

// ─── Server ──────────────────────────────────────────────────────────────────

func runServer(port string) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	listener, err := quic.ListenAddr(":"+port, generateTLSConfig(keyLog), productionQuicConfig())
	if err != nil {
		fmt.Printf("Listen error: %v\n", err)
		return
	}
	defer listener.Close()
	fmt.Printf("[QUIC Server] Listening on port %s (production config: large windows + Allow0RTT)\n", port)

	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			fmt.Printf("Accept error: %v\n", err)
			continue
		}
		go handleConnection(conn)
	}
}

// handleConnection accepts ALL streams on a single QUIC connection and handles each.
func handleConnection(conn *quic.Conn) {
	for {
		stream, err := (*conn).AcceptStream(context.Background())
		if err != nil {
			// Connection closed or idle — normal shutdown
			return
		}
		s := stream // capture concrete *quic.Stream before next iteration
		go func() {
			defer s.Close()
			for {
				msg, err := readMsg(s)
				if err != nil {
					return
				}
				preview := string(msg[:min(50, len(msg))])
				fmt.Printf("[Received] %d bytes: %s\n", len(msg), preview)
				response := append([]byte("ACK_QUIC:"), msg...)
				if err := writeMsg(s, response); err != nil {
					fmt.Printf("Write error: %v\n", err)
					return
				}
			}
		}()
	}
}

// ─── Client: Scenario A – vanilla, fresh connection ──────────────────────────

func runClient(addr, port string, size int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-diagnostic"},
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
	}
	quicCfg := productionQuicConfig()
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[QUIC Client] Connecting to %s  payload=%d bytes\n", target, size)
	conn, err := quic.DialAddr(context.Background(), target, tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	defer conn.CloseWithError(0, "done")
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		fmt.Printf("Stream error: %v\n", err)
		return
	}
	defer stream.Close()
	for i := 1; i <= 10; i++ {
		payload := []byte(makePayload(i, size))
		start := time.Now()
		if err := writeMsg(stream, payload); err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(stream)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			return
		}
		rtt := time.Since(start)
		fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
			i, len(payload), len(resp), rtt)
	}
}

// ─── Client: -wan – timer starts BEFORE quic.DialAddr ───────────────────────

func runClientWAN(addr, port string, size int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	sessionCache := tls.NewLRUClientSessionCache(100)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-diagnostic"},
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
		ClientSessionCache: sessionCache,
	}
	quicCfg := productionQuicConfig()
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[QUIC WAN] → %s  payload=%d bytes\n", target, size)

	// Warmup: full 1-RTT handshake to obtain session ticket
	warmupConn, err := quic.DialAddr(context.Background(), target, tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Warmup error: %v\n", err)
		return
	}
	warmupConn.CloseWithError(0, "warmup")

	// Measure 0-RTT connection + first message
	dialStart := time.Now()
	conn, err := quic.DialAddrEarly(context.Background(), target, tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	dialMs := float64(time.Since(dialStart).Microseconds()) / 1000.0
	defer conn.CloseWithError(0, "done")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		fmt.Printf("Stream error: %v\n", err)
		return
	}
	defer stream.Close()

	payload := []byte(makePayload(1, size))
	msgStart := time.Now()
	if err := writeMsg(stream, payload); err != nil {
		fmt.Printf("Write error: %v\n", err)
		return
	}
	resp, err := readMsg(stream)
	if err != nil {
		fmt.Printf("Read error: %v\n", err)
		return
	}
	msgRTT := time.Since(msgStart)
	totalMs := float64(time.Since(dialStart).Microseconds()) / 1000.0

	fmt.Printf("  DIAL_MS=%.1f  MSG_RTT=%v  TOTAL_MS=%.1f  resp=%d bytes\n",
		dialMs, msgRTT, totalMs, len(resp))
}

// ─── Client: -burst N – N sequential fresh connections ──────────────────────

func runClientBurst(addr, port string, size, n int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	target := net.JoinHostPort(addr, port)
	quicCfg := productionQuicConfig()
	ctx := context.Background()

	sessionCache := tls.NewLRUClientSessionCache(100)

	fmt.Printf("[QUIC BURST n=%d size=%d → %s]\n", n, size, target)

	var sumMs float64
	for i := 1; i <= n; i++ {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-diagnostic"},
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       keyLog,
			ClientSessionCache: sessionCache,
		}

		dialStart := time.Now()

		var conn *quic.Conn
		var err error
		var label string

		if i == 1 {
			// Full 1-RTT handshake — no ticket yet, cannot use 0-RTT
			conn, err = quic.DialAddr(ctx, target, tlsConfig, quicCfg)
			label = "1-RTT"
		} else {
			conn, err = quic.DialAddrEarly(ctx, target, tlsConfig, quicCfg)
			label = "0-RTT"
		}
		if err != nil {
			fmt.Printf("Connect error (conn %d): %v\n", i, err)
			return
		}
		dialMs := float64(time.Since(dialStart).Microseconds()) / 1000.0

		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			fmt.Printf("Stream error (conn %d): %v\n", i, err)
			conn.CloseWithError(1, "")
			return
		}

		payload := []byte(makePayload(i, size))
		msgStart := time.Now()
		if err := writeMsg(stream, payload); err != nil {
			fmt.Printf("Write error (conn %d): %v\n", i, err)
			stream.Close()
			conn.CloseWithError(0, "")
			return
		}
		resp, err := readMsg(stream)
		if err != nil {
			fmt.Printf("Read error (conn %d): %v\n", i, err)
			stream.Close()
			conn.CloseWithError(0, "")
			return
		}
		msgRTT := time.Since(msgStart)
		totalMs := float64(time.Since(dialStart).Microseconds()) / 1000.0
		_ = resp

		fmt.Printf("  [conn %d %s] DIAL_MS=%.1f  MSG_RTT=%v  TOTAL_MS=%.1f\n",
			i, label, dialMs, msgRTT, totalMs)

		sumMs += totalMs
		stream.Close()
		conn.CloseWithError(0, "done")
	}
	fmt.Printf("  SUM_TOTAL_MS=%.1f  AVG_MS=%.1f\n", sumMs, sumMs/float64(n))
}

// ─── Client: -opt – fresh connection per size, shared session cache ───────────

func runClientOpt(addr, port string) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	sessionCache := tls.NewLRUClientSessionCache(100)
	quicCfg := productionQuicConfig()
	for _, size := range sweepSizes {
		fmt.Printf("\n=== SIZE %d ===\n", size)
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-diagnostic"},
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       keyLog,
			ClientSessionCache: sessionCache,
		}
		target := net.JoinHostPort(addr, port)
		fmt.Printf("[QUIC Client OPT] Connecting to %s  payload=%d bytes\n", target, size)
		conn, err := quic.DialAddr(context.Background(), target, tlsConfig, quicCfg)
		if err != nil {
			fmt.Printf("Connect error: %v\n", err)
			return
		}
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			fmt.Printf("Stream error: %v\n", err)
			conn.CloseWithError(1, "")
			return
		}
		for i := 1; i <= 10; i++ {
			payload := []byte(makePayload(i, size))
			start := time.Now()
			if err := writeMsg(stream, payload); err != nil {
				fmt.Printf("Write error: %v\n", err)
				stream.Close()
				conn.CloseWithError(0, "")
				return
			}
			resp, err := readMsg(stream)
			if err != nil {
				fmt.Printf("Read error: %v\n", err)
				stream.Close()
				conn.CloseWithError(0, "")
				return
			}
			rtt := time.Since(start)
			fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
				i, len(payload), len(resp), rtt)
		}
		stream.Close()
		conn.CloseWithError(0, "done")
	}
}

// ─── Client: -reuse – one persistent connection for all sizes ────────────────

func runClientReuse(addr, port string, opt bool) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-diagnostic"},
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
	}
	var quicCfg *quic.Config
	if opt {
		tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(100)
		quicCfg = productionQuicConfig()
	} else {
		quicCfg = &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			KeepAlivePeriod: 10 * time.Second,
		}
	}
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[QUIC Client REUSE opt=%v] Connecting to %s\n", opt, target)
	conn, err := quic.DialAddr(context.Background(), target, tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	defer conn.CloseWithError(0, "done")
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		fmt.Printf("Stream error: %v\n", err)
		return
	}
	defer stream.Close()
	for _, size := range sweepSizes {
		fmt.Printf("\n=== SIZE %d ===\n", size)
		for i := 1; i <= 10; i++ {
			payload := []byte(makePayload(i, size))
			start := time.Now()
			if err := writeMsg(stream, payload); err != nil {
				fmt.Printf("Write error: %v\n", err)
				return
			}
			resp, err := readMsg(stream)
			if err != nil {
				fmt.Printf("Read error: %v\n", err)
				return
			}
			rtt := time.Since(start)
			fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
				i, len(payload), len(resp), rtt)
		}
	}
}
