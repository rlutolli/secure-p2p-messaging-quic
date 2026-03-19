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

// Shared TLS config for session resumption
var clientSessionCache = tls.NewLRUClientSessionCache(100)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run quic_0rtt.go [server|client] [addr] [port]")
		return
	}
	mode := os.Args[1]
	port := "9002"
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
	quicConf := &quic.Config{ Allow0RTT: true } // <--- THE MAGIC SWITCH

	l, err := quic.ListenAddr(":"+port, tlsConf, quicConf)
	if err != nil { panic(err) }
	fmt.Printf("QUIC 0-RTT Server on :%s\n", port)

	for {
		conn, err := l.Accept(context.Background())
		if err != nil { continue }
		
		go func(c *quic.Conn) {
			// Check if this connection used 0-RTT
			is0RTT := c.ConnectionState().TLS.DidResume
			if is0RTT {
				fmt.Println(">> Server accepted a 0-RTT (Resumed) connection!")
			} else {
				fmt.Println(">> Server accepted a 1-RTT (New) connection.")
			}
			
			s, err := c.AcceptStream(context.Background())
			if err != nil { return }
			io.Copy(io.Discard, s)
			time.Sleep(100 * time.Millisecond)
			c.CloseWithError(0, "")
		}(conn)
	}
}

func runClient(addr, port string) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		ClientSessionCache: clientSessionCache,
		NextProtos:         []string{"bench"},
	}
	
	dest := net.JoinHostPort(addr, port)

	fmt.Println("\n--- Run 1: Initial Connection (1-RTT) ---")
	doConnection(dest, tlsConf, false) 

	time.Sleep(500 * time.Millisecond)

	fmt.Println("\n--- Run 2: Resumed Connection (0-RTT) ---")
	doConnection(dest, tlsConf, true)
}

func doConnection(dest string, tlsConf *tls.Config, use0RTT bool) {
	start := time.Now()
	ctx := context.Background()
	var conn *quic.Conn
	var err error

	if use0RTT {
		conn, err = quic.DialAddrEarly(ctx, dest, tlsConf, nil)
	} else {
		conn, err = quic.DialAddr(ctx, dest, tlsConf, nil)
	}
	
	if err != nil { fmt.Printf("Dial Error: %v\n", err); return }
	
	if use0RTT {
		handshakeTime := time.Since(start)
		fmt.Printf("Handshake Time (DialEarly): %v (Instant!)\n", handshakeTime)
		<-conn.HandshakeComplete()
	} else {
		<-conn.HandshakeComplete() 
		handshakeTime := time.Since(start)
		fmt.Printf("Handshake Time: %v\n", handshakeTime)
	}

	s, _ := conn.OpenStreamSync(ctx)
	s.Write([]byte("Hello"))
	s.Close()
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
