// tcp_raw.go – TCP+TLS echo benchmark with SSL key logging for Wireshark
//
// Usage:
//
//		go run tcp_raw.go [-opt] [-reuse] [-wan] [-burst N] server [port]
//		go run tcp_raw.go [-opt] [-reuse] [-wan] [-burst N] client [addr] [port] [size]
//
//		-opt      Enable optimisations: TCP_NODELAY + TLS session cache
//	  -reuse    Run all payload sizes (64, 100, 5000 B) on a single persistent connection
//		-wan      Include handshake in timing; timer starts before tls.Dial (Scenario 2)
//		-burst N  Make N short-lived fresh connections sequentially (Scenario 6)
//
// Payload format:
//
//	"MSG1:TCP:SIZE=64:AAAA..."   padded with 'A' to exactly <size> bytes
//
// Set SSLKEYLOGFILE=/path/to/tls_keys.log so Wireshark can decrypt traffic.
package main

import (
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
)

const (
	DefaultPort    = "9000"
	KeyLogFallback = "tls_keys.txt"
)

var (
	flagOpt   = flag.Bool("opt", false, "enable optimisations: TCP_NODELAY + TLS session cache")
	flagReuse = flag.Bool("reuse", false, "run all sizes (64,100,5000) on one persistent connection")
	flagWAN   = flag.Bool("wan", false, "include handshake in timing; timer starts before tls.Dial")
	flagBurst = flag.Int("burst", 0, "make N short-lived connections sequentially (reconnect scenario)")
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
	header := fmt.Sprintf("MSG%d:TCP:SIZE=%d:", msgNum, size)
	if len(header) >= size {
		return header[:size]
	}
	return header + strings.Repeat("A", size-len(header))
}

// writeMsg sends a 4-byte big-endian length prefix + body in a SINGLE Write call
// to avoid Nagle-induced latency from split writes.
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

// generateTLSConfig builds an ephemeral self-signed TLS 1.3 config.
func generateTLSConfig(keyLog io.Writer) *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"TCP+TLS Benchmark"}},
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
		MinVersion:   tls.VersionTLS13,
		KeyLogWriter: keyLog,
	}
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: tcp_raw [-opt] [-reuse] [-wan] [-burst N] server [port]")
		fmt.Println("       tcp_raw [-opt] [-reuse] [-wan] [-burst N] client [addr] [port] [size] [msgs]")
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
		msgs := 10
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
		if len(args) >= 5 {
			n, err := strconv.Atoi(args[4])
			if err != nil || n < 1 {
				fmt.Printf("Invalid msgs %q\n", args[4])
				os.Exit(1)
			}
			msgs = n
		}

		switch {
		case *flagBurst > 0:
			runClientBurst(addr, port, size, *flagBurst)
		case *flagWAN:
			runClientWAN(addr, port, size)
		case *flagReuse:
			runClientReuse(addr, port, *flagOpt, msgs)
		case *flagOpt:
			runClientOpt(addr, port, msgs)
		default:
			runClient(addr, port, size, msgs)
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
	listener, err := tls.Listen("tcp", ":"+port, generateTLSConfig(keyLog))
	if err != nil {
		fmt.Printf("Listen error: %v\n", err)
		return
	}
	defer listener.Close()
	fmt.Printf("[TCP+TLS Server] Listening on port %s\n", port)
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("Accept error: %v\n", err)
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	for {
		msg, err := readMsg(conn)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Read error: %v\n", err)
			}
			return
		}
		preview := string(msg[:min(50, len(msg))])
		fmt.Printf("[Received] %d bytes: %s\n", len(msg), preview)
		response := append([]byte("ACK_TLS:"), msg...)
		if err := writeMsg(conn, response); err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}
	}
}

// ─── Client: Scenario A – vanilla, fresh connection ──────────────────────────

func runClient(addr, port string, size int, msgs int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
	}
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[TCP+TLS Client] Connecting to %s  payload=%d bytes\n", target, size)
	conn, err := tls.Dial("tcp", target, tlsConfig)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	defer conn.Close()
	for i := 1; i <= msgs; i++ {
		payload := []byte(makePayload(i, size))
		start := time.Now()
		if err := writeMsg(conn, payload); err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(conn)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			return
		}
		rtt := time.Since(start)
		fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
			i, len(payload), len(resp), rtt)
		// delay removed
	}
}

// ─── Client: -wan – timer starts BEFORE tls.Dial ────────────────────────────

func runClientWAN(addr, port string, size int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
	}
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[TCP WAN] → %s  payload=%d bytes\n", target, size)

	dialStart := time.Now()
	conn, err := tls.Dial("tcp", target, tlsConfig)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	dialMs := float64(time.Since(dialStart).Microseconds()) / 1000.0
	defer conn.Close()

	payload := []byte(makePayload(1, size))
	msgStart := time.Now()
	if err := writeMsg(conn, payload); err != nil {
		fmt.Printf("Write error: %v\n", err)
		return
	}
	resp, err := readMsg(conn)
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

	sessionCache := tls.NewLRUClientSessionCache(100)

	fmt.Printf("[TCP BURST n=%d size=%d → %s]\n", n, size, target)

	var sumMs float64
	for i := 1; i <= n; i++ {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       keyLog,
			ClientSessionCache: sessionCache,
		}

		dialStart := time.Now()
		conn, err := tls.Dial("tcp", target, tlsConfig)
		if err != nil {
			fmt.Printf("Connect error: %v\n", err)
			return
		}
		dialMs := float64(time.Since(dialStart).Microseconds()) / 1000.0

		payload := []byte(makePayload(i, size))
		msgStart := time.Now()
		if err := writeMsg(conn, payload); err != nil {
			fmt.Printf("Write error: %v\n", err)
			conn.Close()
			return
		}
		resp, err := readMsg(conn)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			conn.Close()
			return
		}
		msgRTT := time.Since(msgStart)
		totalMs := float64(time.Since(dialStart).Microseconds()) / 1000.0
		_ = resp

		fmt.Printf("  [conn %d] DIAL_MS=%.1f  MSG_RTT=%v  TOTAL_MS=%.1f\n",
			i, dialMs, msgRTT, totalMs)

		sumMs += totalMs
		conn.Close()
	}
	fmt.Printf("  SUM_TOTAL_MS=%.1f  AVG_MS=%.1f\n", sumMs, sumMs/float64(n))
}

// ─── Client: -opt – fresh connection per size, shared session cache ───────────

func runClientOpt(addr, port string, msgs int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	sessionCache := tls.NewLRUClientSessionCache(100)
	for _, size := range sweepSizes {
		fmt.Printf("\n=== SIZE %d ===\n", size)
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			KeyLogWriter:       keyLog,
			ClientSessionCache: sessionCache,
		}
		target := net.JoinHostPort(addr, port)
		fmt.Printf("[TCP+TLS Client OPT] Connecting to %s  payload=%d bytes\n", target, size)
		conn, err := tls.Dial("tcp", target, tlsConfig)
		if err != nil {
			fmt.Printf("Connect error: %v\n", err)
			return
		}
		if tc, ok := conn.NetConn().(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		for i := 1; i <= msgs; i++ {
			payload := []byte(makePayload(i, size))
			start := time.Now()
			if err := writeMsg(conn, payload); err != nil {
				fmt.Printf("Write error: %v\n", err)
				conn.Close()
				return
			}
			resp, err := readMsg(conn)
			if err != nil {
				fmt.Printf("Read error: %v\n", err)
				conn.Close()
				return
			}
			rtt := time.Since(start)
			fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
				i, len(payload), len(resp), rtt)
			// delay removed
		}
		conn.Close()
	}
}

// ─── Client: -reuse – one persistent connection for all sizes ────────────────

func runClientReuse(addr, port string, opt bool, msgs int) {
	keyLog := openKeyLog()
	if keyLog != nil {
		defer keyLog.Close()
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		KeyLogWriter:       keyLog,
	}
	if opt {
		tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(100)
	}
	target := net.JoinHostPort(addr, port)
	fmt.Printf("[TCP+TLS Client REUSE opt=%v] Connecting to %s\n", opt, target)
	conn, err := tls.Dial("tcp", target, tlsConfig)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}
	defer conn.Close()
	if opt {
		if tc, ok := conn.NetConn().(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
	}
	for _, size := range sweepSizes {
		fmt.Printf("\n=== SIZE %d ===\n", size)
		for i := 1; i <= msgs; i++ {
			payload := []byte(makePayload(i, size))
			start := time.Now()
			if err := writeMsg(conn, payload); err != nil {
				fmt.Printf("Write error: %v\n", err)
				return
			}
			resp, err := readMsg(conn)
			if err != nil {
				fmt.Printf("Read error: %v\n", err)
				return
			}
			rtt := time.Since(start)
			fmt.Printf("  [%d] sent=%d bytes  response=%d bytes  RTT=%v\n",
				i, len(payload), len(resp), rtt)
			// delay removed
		}
	}
}
