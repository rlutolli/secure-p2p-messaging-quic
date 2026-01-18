package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

/*
 * Isolated Transport Benchmarks
 *
 * These benchmarks compare raw performance of:
 * 1. Plain TCP (unencrypted) - baseline
 * 2. TCP + TLS 1.3 (encrypted) - what our app uses
 * 3. QUIC (encrypted by design) - what our app uses by default
 *
 * This helps verify that TLS is actually adding overhead (proving encryption works)
 * and understand the true performance characteristics of each transport.
 */

// ============================================================================
// BENCHMARK: Plain TCP (Unencrypted) - Baseline
// ============================================================================

func BenchmarkRawTCP_Unencrypted(b *testing.B) {
	// Start plain TCP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Failed to start TCP server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Server goroutine - echo back
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(conn)
		}
	}()

	// Client connects
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	msg := []byte("BenchmarkPayload1234567890123456789012345678901234567890")
	buf := make([]byte, len(msg))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := conn.Write(msg)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		_, err = conn.Read(buf)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
	}
}

// ============================================================================
// BENCHMARK: TCP + TLS 1.3 (Encrypted) - What our app uses for --use-tcp
// ============================================================================

func BenchmarkTCP_TLS13(b *testing.B) {
	tlsConfig := generateTLSConfig()

	// Start TLS TCP server
	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		b.Fatalf("Failed to start TLS server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Server goroutine - echo back
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(conn)
		}
	}()

	// Client connects with TLS
	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
		MinVersion:         tls.VersionTLS13,
	}

	conn, err := tls.Dial("tcp", addr, clientTLSConfig)
	if err != nil {
		b.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Verify TLS 1.3 is active
	state := conn.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		b.Fatalf("Expected TLS 1.3, got version: %x", state.Version)
	}
	b.Logf("TLS Version: %x (TLS 1.3 = 0x304), CipherSuite: %x", state.Version, state.CipherSuite)

	msg := []byte("BenchmarkPayload1234567890123456789012345678901234567890")
	buf := make([]byte, len(msg))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := conn.Write(msg)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		_, err = conn.Read(buf)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
	}
}

// ============================================================================
// BENCHMARK: QUIC (Encrypted by design) - What our app uses by default
// ============================================================================

func BenchmarkQUIC_Encrypted(b *testing.B) {
	tlsConfig := generateTLSConfig()

	// Start QUIC server
	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		b.Fatalf("Failed to start QUIC server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Server goroutine - echo back
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				defer c.CloseWithError(0, "done")
				stream, err := c.AcceptStream(context.Background())
				if err != nil {
					return
				}
				buf := make([]byte, 1024)
				for {
					n, err := stream.Read(buf)
					if err != nil {
						return
					}
					stream.Write(buf[:n])
				}
			}(conn)
		}
	}()

	// Give server time to start
	time.Sleep(10 * time.Millisecond)

	// Client connects
	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr, clientTLSConfig, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		b.Fatalf("Failed to connect: %v", err)
	}
	defer conn.CloseWithError(0, "done")

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		b.Fatalf("Failed to open stream: %v", err)
	}

	msg := []byte("BenchmarkPayload1234567890123456789012345678901234567890")
	buf := make([]byte, len(msg))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := stream.Write(msg)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		_, err = stream.Read(buf)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
	}
}

// ============================================================================
// BENCHMARK: Connection Establishment Time
// ============================================================================

func BenchmarkConnectionTime_RawTCP(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Failed to start server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		conn.Close()
	}
}

func BenchmarkConnectionTime_TCPTLS(b *testing.B) {
	tlsConfig := generateTLSConfig()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		b.Fatalf("Failed to start server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Force handshake completion
			tlsConn := conn.(*tls.Conn)
			tlsConn.Handshake()
			conn.Close()
		}
	}()

	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := tls.Dial("tcp", addr, clientTLSConfig)
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		conn.Close()
	}
}

func BenchmarkConnectionTime_QUIC(b *testing.B) {
	tlsConfig := generateTLSConfig()

	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, &quic.Config{
		MaxIdleTimeout: 30 * time.Second,
	})
	if err != nil {
		b.Fatalf("Failed to start server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			conn.CloseWithError(0, "done")
		}
	}()

	time.Sleep(10 * time.Millisecond)

	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := quic.DialAddr(ctx, addr, clientTLSConfig, &quic.Config{
			MaxIdleTimeout: 30 * time.Second,
		})
		cancel()
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		conn.CloseWithError(0, "done")
	}
}

// ============================================================================
// HELPER: Print TLS verification info
// ============================================================================

func TestVerifyTLSEncryption(t *testing.T) {
	// This test verifies that TLS 1.3 is actually being used

	tlsConfig := generateTLSConfig()

	// Start TLS server
	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatalf("Failed to start TLS server: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		tlsConn := conn.(*tls.Conn)
		tlsConn.Handshake()

		state := tlsConn.ConnectionState()
		fmt.Printf("[SERVER] TLS Version: 0x%x (TLS 1.3 = 0x304)\n", state.Version)
		fmt.Printf("[SERVER] CipherSuite: 0x%x\n", state.CipherSuite)
		fmt.Printf("[SERVER] HandshakeComplete: %v\n", state.HandshakeComplete)
	}()

	// Client
	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}

	conn, err := tls.Dial("tcp", addr, clientTLSConfig)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	state := conn.ConnectionState()
	fmt.Printf("[CLIENT] TLS Version: 0x%x (TLS 1.3 = 0x304)\n", state.Version)
	fmt.Printf("[CLIENT] CipherSuite: 0x%x\n", state.CipherSuite)
	fmt.Printf("[CLIENT] NegotiatedProtocol: %s\n", state.NegotiatedProtocol)

	if state.Version != tls.VersionTLS13 {
		t.Errorf("Expected TLS 1.3 (0x304), got: 0x%x", state.Version)
	}

	<-serverDone
	t.Log("TLS 1.3 verified successfully!")
}
