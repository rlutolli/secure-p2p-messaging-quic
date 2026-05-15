// speed_bench.go – Zero-overhead QUIC vs TCP+TLS speed benchmark
//
// Usage:
//
//	go run speed_bench.go server [port]
//	go run speed_bench.go client [addr] [port] [size] [count] [mode]
//
//	mode:  "persistent" = one connection, N messages back-to-back
//	       "fresh"      = N fresh connections, 1 message each (0-RTT for QUIC)
//
// This benchmark strips ALL printf/logging from the echo hot path,
// uses sync.Pool for buffers, and pre-computes payloads.
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
	"math"
	"math/big"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const defaultPort = "9000"

var flagProto = flag.String("proto", "quic", "protocol: quic or tcp")

// ─── Shared helpers ──────────────────────────────────────────────────────────

var writeBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4+5000)
		return &b
	},
}

var readHdrPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4)
		return &b
	},
}

// writeMsg sends a 4-byte length prefix + body using a pooled buffer.
func writeMsg(w io.Writer, msg []byte) error {
	bufPtr := writeBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:4+len(msg)]
	binary.BigEndian.PutUint32(buf[:4], uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	writeBufPool.Put(bufPtr)
	return err
}

// readMsg reads a length-prefixed message into dst (reuses dst if cap allows).
func readMsg(r io.Reader, dst []byte) ([]byte, error) {
	hdrPtr := readHdrPool.Get().(*[]byte)
	hdr := *hdrPtr
	if _, err := io.ReadFull(r, hdr); err != nil {
		readHdrPool.Put(hdrPtr)
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr)
	readHdrPool.Put(hdrPtr)
	if cap(dst) < int(size) {
		dst = make([]byte, size)
	} else {
		dst = dst[:size]
	}
	_, err := io.ReadFull(r, dst)
	return dst, err
}

// makePayload creates a payload of exactly `size` bytes.
func makePayload(msgNum, size int) []byte {
	header := []byte(fmt.Sprintf("MSG%d:SIZE=%d:", msgNum, size))
	if len(header) >= size {
		return header[:size]
	}
	buf := make([]byte, size)
	copy(buf, header)
	for i := len(header); i < size; i++ {
		buf[i] = 'A'
	}
	return buf
}

// generateTLSConfig builds an ephemeral self-signed TLS 1.3 config.
func generateTLSConfig() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"SpeedBench"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"speed"}}
}

// ─── QUIC Server ─────────────────────────────────────────────────────────────

func runQUICServer(port string) {
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 5 * time.Minute,
		MaxIncomingStreams:             1000,
		MaxIncomingUniStreams:          1000,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
		Allow0RTT:                      true,
	}
	listener, err := quic.ListenAddr(":"+port, generateTLSConfig(), quicCfg)
	if err != nil {
		fmt.Printf("Listen error: %v\n", err)
		return
	}
	defer listener.Close()
	fmt.Printf("[QUIC Server] :%s (0-RTT, pooled buffers, zero-log echo)\n", port)

	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			continue
		}
		go handleQUICConn(conn)
	}
}

func handleQUICConn(conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		s := stream
		go handleQUICStream(s)
	}
}

func handleQUICStream(s *quic.Stream) {
	defer s.Close()
	buf := make([]byte, 0, 5000)
	for {
		msg, err := readMsg(s, buf)
		if err != nil {
			return
		}
		buf = msg
		resp := append([]byte("ACK:"), msg...)
		if err := writeMsg(s, resp); err != nil {
			return
		}
	}
}

// ─── TCP Server ──────────────────────────────────────────────────────────────

func runTCPServer(port string) {
	listener, err := tls.Listen("tcp", ":"+port, generateTLSConfig())
	if err != nil {
		fmt.Printf("Listen error: %v\n", err)
		return
	}
	defer listener.Close()
	fmt.Printf("[TCP Server]  :%s (pooled buffers, zero-log echo)\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleTCPConn(conn)
	}
}

func handleTCPConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 0, 5000)
	for {
		msg, err := readMsg(conn, buf)
		if err != nil {
			return
		}
		buf = msg
		resp := append([]byte("ACK:"), msg...)
		if err := writeMsg(conn, resp); err != nil {
			return
		}
	}
}

// ─── QUIC Client: Persistent ─────────────────────────────────────────────────

func runQUICClientPersistent(addr, port string, size, count int) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"speed"},
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 5 * time.Minute,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
	}
	conn, err := quic.DialAddr(context.Background(), net.JoinHostPort(addr, port), tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Dial error: %v\n", err)
		return
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		fmt.Printf("Stream error: %v\n", err)
		return
	}
	defer stream.Close()

	payload := makePayload(1, size)
	buf := make([]byte, 0, 5000)
	results := make([]time.Duration, count)

	for i := 0; i < count; i++ {
		start := time.Now()
		if err := writeMsg(stream, payload); err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(stream, buf)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			return
		}
		buf = resp
		results[i] = time.Since(start)
	}

	printResults("QUIC persistent", size, count, results)
}

// ─── QUIC Client: Fresh (0-RTT after warmup) ─────────────────────────────────

func runQUICClientFresh(addr, port string, size, count int) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"speed"},
		ClientSessionCache: tls.NewLRUClientSessionCache(100),
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 5 * time.Minute,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
	}
	target := net.JoinHostPort(addr, port)

	// Warmup to get session ticket
	warmupConn, err := quic.DialAddr(context.Background(), target, tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Warmup error: %v\n", err)
		return
	}
	warmupConn.CloseWithError(0, "warmup")

	payload := makePayload(1, size)
	buf := make([]byte, 0, 5000)
	results := make([]time.Duration, count)

	for i := 0; i < count; i++ {
		start := time.Now()
		conn, err := quic.DialAddrEarly(context.Background(), target, tlsConfig, quicCfg)
		if err != nil {
			fmt.Printf("Dial error: %v\n", err)
			return
		}
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			conn.CloseWithError(0, "")
			fmt.Printf("Stream error: %v\n", err)
			return
		}
		if err := writeMsg(stream, payload); err != nil {
			stream.Close()
			conn.CloseWithError(0, "")
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(stream, buf)
		if err != nil {
			stream.Close()
			conn.CloseWithError(0, "")
			fmt.Printf("Read error: %v\n", err)
			return
		}
		buf = resp
		stream.Close()
		conn.CloseWithError(0, "")
		results[i] = time.Since(start)
	}

	printResults("QUIC fresh (0-RTT)", size, count, results)
}

// ─── TCP Client: Persistent ──────────────────────────────────────────────────

func runTCPClientPersistent(addr, port string, size, count int) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	conn, err := tls.Dial("tcp", net.JoinHostPort(addr, port), tlsConfig)
	if err != nil {
		fmt.Printf("Dial error: %v\n", err)
		return
	}
	defer conn.Close()

	payload := makePayload(1, size)
	buf := make([]byte, 0, 5000)
	results := make([]time.Duration, count)

	for i := 0; i < count; i++ {
		start := time.Now()
		if err := writeMsg(conn, payload); err != nil {
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(conn, buf)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			return
		}
		buf = resp
		results[i] = time.Since(start)
	}

	printResults("TCP persistent", size, count, results)
}

// ─── TCP Client: Fresh ───────────────────────────────────────────────────────

func runTCPClientFresh(addr, port string, size, count int) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	payload := makePayload(1, size)
	buf := make([]byte, 0, 5000)
	results := make([]time.Duration, count)

	for i := 0; i < count; i++ {
		start := time.Now()
		conn, err := tls.Dial("tcp", net.JoinHostPort(addr, port), tlsConfig)
		if err != nil {
			fmt.Printf("Dial error: %v\n", err)
			return
		}
		if err := writeMsg(conn, payload); err != nil {
			conn.Close()
			fmt.Printf("Write error: %v\n", err)
			return
		}
		resp, err := readMsg(conn, buf)
		if err != nil {
			conn.Close()
			fmt.Printf("Read error: %v\n", err)
			return
		}
		buf = resp
		conn.Close()
		results[i] = time.Since(start)
	}

	printResults("TCP fresh", size, count, results)
}

// ─── Results printer ─────────────────────────────────────────────────────────

func printResults(label string, size, count int, results []time.Duration) {
	var sum time.Duration
	for _, d := range results {
		sum += d
	}
	avg := sum / time.Duration(count)

	sorted := make([]time.Duration, count)
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 := sorted[count/2]
	p99 := sorted[int(math.Ceil(float64(count)*0.99))-1]
	min := sorted[0]
	max := sorted[count-1]

	fmt.Printf("%-22s  size=%-5d  count=%-5d  min=%-10v  p50=%-10v  p99=%-10v  max=%-10v  avg=%v\n",
		label, size, count, min, p50, p99, max, avg)
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: speed_bench server [port]")
		fmt.Println("       speed_bench client [addr] [port] [size] [count] [mode]")
		fmt.Println("       mode: persistent | fresh")
		os.Exit(1)
	}

	switch args[0] {
	case "server":
		port := defaultPort
		if len(args) >= 2 {
			port = args[1]
		}
		if *flagProto == "tcp" {
			runTCPServer(port)
		} else {
			runQUICServer(port)
		}

	case "client":
		addr := "127.0.0.1"
		port := defaultPort
		size := 64
		count := 10
		mode := "persistent"
		if len(args) >= 2 {
			addr = args[1]
		}
		if len(args) >= 3 {
			port = args[2]
		}
		if len(args) >= 4 {
			size, _ = strconv.Atoi(args[3])
		}
		if len(args) >= 5 {
			count, _ = strconv.Atoi(args[4])
		}
		if len(args) >= 6 {
			mode = args[5]
		}

		fmt.Printf("\n=== BENCHMARK  proto=%s  size=%d  count=%d  mode=%s ===\n\n",
			*flagProto, size, count, mode)
		fmt.Printf("%-22s  size     count    min          p50          p99          max          avg\n", "MODE")
		fmt.Println("------------------------------------------------------------------------------------------------------------------------")

		if *flagProto == "tcp" {
			if mode == "persistent" {
				runTCPClientPersistent(addr, port, size, count)
			} else {
				runTCPClientFresh(addr, port, size, count)
			}
		} else {
			if mode == "persistent" {
				runQUICClientPersistent(addr, port, size, count)
			} else {
				runQUICClientFresh(addr, port, size, count)
			}
		}
		fmt.Println()

	default:
		fmt.Printf("Unknown mode: %s\n", args[0])
		os.Exit(1)
	}
}
