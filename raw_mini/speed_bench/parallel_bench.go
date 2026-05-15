// parallel_bench.go – Parallel stream benchmark: QUIC multiplex vs TCP many-connections
//
// Tests total elapsed time to send N echo messages in parallel:
//
//	QUIC: 1 connection, N concurrent streams
//	TCP:  N concurrent connections
//
// Usage:
//
//	go run parallel_bench.go server [port]
//	go run parallel_bench.go client [addr] [port] [size] [parallel]
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
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

var flagProto = flag.String("proto", "quic", "protocol: quic or tcp")

func generateTLSConfig() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"ParallelBench"}},
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
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"parallel"}}
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
	size := binary.BigEndian.Uint32(hdr)
	buf := make([]byte, size)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func makePayload(idx, size int) []byte {
	header := []byte(fmt.Sprintf("P%d:", idx))
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

// ─── QUIC Server ─────────────────────────────────────────────────────────────

func runQUICServer(port string) {
	quicCfg := &quic.Config{
		MaxIdleTimeout:             5 * time.Minute,
		MaxIncomingStreams:         1000,
		InitialStreamReceiveWindow: 6 * 1024 * 1024,
		MaxStreamReceiveWindow:     16 * 1024 * 1024,
		MaxConnectionReceiveWindow: 64 * 1024 * 1024,
		Allow0RTT:                  true,
	}
	listener, _ := quic.ListenAddr(":"+port, generateTLSConfig(), quicCfg)
	defer listener.Close()
	fmt.Printf("[QUIC Server] :%s\n", port)

	for {
		conn, _ := listener.Accept(context.Background())
		go func(c *quic.Conn) {
			for {
				stream, err := c.AcceptStream(context.Background())
				if err != nil {
					return
				}
				s := stream
				go func() {
					defer s.Close()
					for {
						msg, err := readMsg(s)
						if err != nil {
							return
						}
						resp := append([]byte("ACK:"), msg...)
						writeMsg(s, resp)
					}
				}()
			}
		}(conn)
	}
}

// ─── TCP Server ──────────────────────────────────────────────────────────────

func runTCPServer(port string) {
	listener, _ := tls.Listen("tcp", ":"+port, generateTLSConfig())
	defer listener.Close()
	fmt.Printf("[TCP Server]  :%s\n", port)

	for {
		conn, _ := listener.Accept()
		go func(c net.Conn) {
			defer c.Close()
			for {
				msg, err := readMsg(c)
				if err != nil {
					return
				}
				resp := append([]byte("ACK:"), msg...)
				writeMsg(c, resp)
			}
		}(conn)
	}
}

// ─── QUIC Client (Parallel Streams) ──────────────────────────────────────────

func runQUICClient(addr, port string, size, parallel int) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"parallel"},
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:             5 * time.Minute,
		InitialStreamReceiveWindow: 6 * 1024 * 1024,
		MaxStreamReceiveWindow:     16 * 1024 * 1024,
		MaxConnectionReceiveWindow: 64 * 1024 * 1024,
	}
	conn, err := quic.DialAddr(context.Background(), net.JoinHostPort(addr, port), tlsConfig, quicCfg)
	if err != nil {
		fmt.Printf("Dial error: %v\n", err)
		return
	}
	defer conn.CloseWithError(0, "")

	payload := makePayload(1, size)
	results := make([]time.Duration, parallel)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			stream, err := conn.OpenStreamSync(context.Background())
			if err != nil {
				return
			}
			defer stream.Close()
			msgStart := time.Now()
			writeMsg(stream, payload)
			readMsg(stream)
			results[idx] = time.Since(msgStart)
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	var sum time.Duration
	for _, d := range results {
		sum += d
	}
	fmt.Printf("QUIC parallel  size=%-5d  parallel=%-4d  total_elapsed=%-12v  avg_rtt=%v\n",
		size, parallel, total, sum/time.Duration(parallel))
}

// ─── TCP Client (Parallel Connections) ───────────────────────────────────────

func runTCPClient(addr, port string, size, parallel int) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	payload := makePayload(1, size)
	results := make([]time.Duration, parallel)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msgStart := time.Now()
			conn, err := tls.Dial("tcp", net.JoinHostPort(addr, port), tlsConfig)
			if err != nil {
				return
			}
			writeMsg(conn, payload)
			readMsg(conn)
			conn.Close()
			results[idx] = time.Since(msgStart)
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	var sum time.Duration
	for _, d := range results {
		sum += d
	}
	fmt.Printf("TCP parallel   size=%-5d  parallel=%-4d  total_elapsed=%-12v  avg_rtt=%v\n",
		size, parallel, total, sum/time.Duration(parallel))
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: parallel_bench server [port]")
		fmt.Println("       parallel_bench client [addr] [port] [size] [parallel]")
		os.Exit(1)
	}

	switch args[0] {
	case "server":
		port := "9200"
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
		port := "9200"
		size := 64
		parallel := 8
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
			parallel, _ = strconv.Atoi(args[4])
		}
		fmt.Printf("\n=== PARALLEL BENCHMARK  proto=%s  size=%d  parallel=%d ===\n\n",
			*flagProto, size, parallel)
		if *flagProto == "tcp" {
			runTCPClient(addr, port, size, parallel)
		} else {
			runQUICClient(addr, port, size, parallel)
		}
		fmt.Println()
	}
}
