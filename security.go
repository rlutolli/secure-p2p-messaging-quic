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
	"log"
	"math/big"
	"net"
	"os"
	"time"
)

var globalKeyLog io.WriteCloser

func init() {
	path := os.Getenv("SSLKEYLOGFILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("[TLS] WARNING: cannot open SSLKEYLOGFILE %q: %v", path, err)
		return
	}
	globalKeyLog = f
	log.Printf("[TLS] Key logging enabled → %s", path)
}

func generateTLSConfig() *tls.Config {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("failed to generate private key: %v", err))
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	localIPs := collectLocalIPs()

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"P2P Messenger"},
			CommonName:   "p2p-messenger",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           localIPs,
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		panic(fmt.Sprintf("failed to create certificate: %v", err))
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal private key: %v", err))
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(fmt.Sprintf("failed to create TLS certificate: %v", err))
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"p2p-messenger/1.0"},
		MinVersion:   tls.VersionTLS13,
	}
	if globalKeyLog != nil {
		cfg.KeyLogWriter = globalKeyLog
	}
	return cfg
}

func collectLocalIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	return ips
}

type RoomCrypto struct {
	roomKey []byte
}

func DeriveRoomKey(roomName, password string) (*RoomCrypto, error) {
	return &RoomCrypto{}, nil
}
