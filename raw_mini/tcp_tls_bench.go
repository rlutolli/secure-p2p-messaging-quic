// tcp_tls_bench.go - TCP+TLS 1.3 one-way 10 MB bulk (fair encrypted comparison vs QUIC).
//
//	go run tcp_tls_bench.go server [port]
//	go run tcp_tls_bench.go client [addr] [port]
package main

import (
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
)

func tlsConf() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{Organization: []string{"bulk"}}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	cert, _ := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("server|client")
		return
	}
	port := "9002"
	if os.Args[1] == "server" {
		if len(os.Args) >= 3 {
			port = os.Args[2]
		}
		l, _ := tls.Listen("tcp", ":"+port, tlsConf())
		fmt.Printf("TCP+TLS server :%s\n", port)
		for {
			c, _ := l.Accept()
			go func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) }(c)
		}
	} else {
		addr := "127.0.0.1"
		if len(os.Args) >= 3 {
			addr = os.Args[2]
		}
		if len(os.Args) >= 4 {
			port = os.Args[3]
		}
		c, err := tls.Dial("tcp", net.JoinHostPort(addr, port), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
		if err != nil {
			fmt.Printf("dial: %v\n", err)
			return
		}
		defer c.Close()
		data := make([]byte, 10*1024*1024)
		t := time.Now()
		c.Write(data)
		d := time.Since(t)
		fmt.Printf("TCP+TLS 10MB Send: %v (%.0f MB/s)\n", d, 10.0/d.Seconds())
	}
}
