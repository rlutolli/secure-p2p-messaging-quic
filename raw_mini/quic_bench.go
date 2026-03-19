package main

import (
"context"
"crypto/ecdsa"
"crypto/elliptic"
"crypto/rand"
"crypto/tls"
"crypto/x509"
"crypto/x509/pkix"
"encoding/pem"
"fmt"
"io"
"math/big"
"net"
"os"
"time"

"github.com/quic-go/quic-go"
)

// Session cache is required for 0-RTT resumption
var clientSessionCache = tls.NewLRUClientSessionCache(100)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run quic_bench.go [server|client] [addr] [port]")
		return
	}
	mode := os.Args[1]
	port := "9001"
	if mode == "server" {
		if len(os.Args) >= 3 { port = os.Args[2] }
		runServer(port)
	} else {
		addr := "172.20.0.10"
		if len(os.Args) >= 3 { addr = os.Args[2] }
		if len(os.Args) >= 4 { port = os.Args[3] }
		runClient(addr, port)
	}
}

func runServer(port string) {
	tlsConf := generateTLSConfig()
	quicConf := &quic.Config{
		// 0-RTT ENABLED
		Allow0RTT: true,
		
		// OPTIMIZED WINDOWS (From previous step)
		InitialStreamReceiveWindow:     1024 * 1024 * 10,
		InitialConnectionReceiveWindow: 1024 * 1024 * 15,
		MaxStreamReceiveWindow:         1024 * 1024 * 20,
		MaxConnectionReceiveWindow:     1024 * 1024 * 30,
		MaxIdleTimeout:                 30 * time.Second,
	}
	l, err := quic.ListenAddr(":"+port, tlsConf, quicConf)
	if err != nil { fmt.Printf("Listen error: %v\n", err); return }
	fmt.Printf("QUIC Server (0-RTT Enabled) on :%s\n", port)
	for {
		conn, err := l.Accept(context.Background())
		if err != nil { continue }
		go func(c *quic.Conn) {
			s, err := (*c).AcceptStream(context.Background())
			if err != nil { return }
			io.Copy(io.Discard, s)
			time.Sleep(100 * time.Millisecond)
			(*c).CloseWithError(0, "")
		}(conn)
	}
}

func runClient(addr, port string) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, 
		NextProtos: []string{"bench"},
		ClientSessionCache: clientSessionCache, // Required for resumption
	}
	
	quicConf := &quic.Config{
		InitialStreamReceiveWindow:     1024 * 1024 * 10,
		InitialConnectionReceiveWindow: 1024 * 1024 * 15,
		MaxIdleTimeout:                 30 * time.Second,
	}
	dest := net.JoinHostPort(addr, port)

	// STEP 1: WARMUP (Necessary to get the Session Ticket)
	// We don't measure this one, it's the "First Visit" cost.
	fmt.Print("Establishing Session (Warmup)... ")
	warmupConn, err := quic.DialAddr(context.Background(), dest, tlsConf, quicConf)
	if err == nil {
		// Keep connection open briefly to receive the Session Ticket!
		// The server sends it AFTER handshake. If we close too fast, we miss it.
		time.Sleep(1 * time.Second) 
		warmupConn.CloseWithError(0, "warmup")
		fmt.Println("Done.")
	} else {
		fmt.Printf("Warmup failed: %v\n", err)
		return
	}

	// STEP 2: MEASURE 0-RTT PERFORMANCE
	// This is the "Second Visit" (Resumed Session)
	start := time.Now()
	conn, err := quic.DialAddrEarly(context.Background(), dest, tlsConf, quicConf)
	
	if err != nil { fmt.Printf("Error: %v\n", err); return }
	defer conn.CloseWithError(0, "")

	// In 0-RTT, we can send immediately. 
	// The effective handshake time is effectively 0 or just local processing time.
	handshake := time.Since(start)
	fmt.Printf("QUIC 0-RTT Handshake: %v", handshake)
	
	// Wait for full handshake just for safety before heavy transfer, 
	// though 0-RTT allows early data.
	<-conn.HandshakeComplete()
	isResumed := conn.ConnectionState().TLS.DidResume
	fmt.Printf("   -> Handshake Complete. Session Resumed: %v\n", isResumed)

	// Throughput: Send 10MB
	s, err := conn.OpenStreamSync(context.Background())
	if err != nil { fmt.Printf("Stream error: %v\n", err); return }
	
	data := make([]byte, 1024*1024*10)
	start = time.Now()
	_, err = s.Write(data)
	if err != nil { fmt.Printf("Write error: %v\n", err); return }
	s.Close()
	
	io.Copy(io.Discard, s)
	duration := time.Since(start)
	fmt.Printf("QUIC 10MB Send (Transfer): %v\n", duration)
}

func generateTLSConfig() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Bench"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("172.20.0.10"), net.ParseIP("172.20.0.11")},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"bench"}}
}
