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
	"math/big"
	"net"
	"time"
)

// generateTLSConfig creates a self-signed certificate for QUIC
func generateTLSConfig() *tls.Config {
	// Use ECDSA P-256 (more efficient than RSA for TLS 1.3)
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("failed to generate private key: %v", err))
	}

	// Create certificate template
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"P2P Messenger"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour), // Short-lived cert
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}

	// Self-sign the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		panic(fmt.Sprintf("failed to create certificate: %v", err))
	}

	// Encode to PEM
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

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"p2p-messenger/1.0"},
		MinVersion:   tls.VersionTLS13, // QUIC requires TLS 1.3
	}
}

// For future: Password-derived room encryption
type RoomCrypto struct {
	roomKey []byte
}

// DeriveRoomKey creates a key from room name + password
// This would be used for end-to-end encryption within rooms
func DeriveRoomKey(roomName, password string) (*RoomCrypto, error) {
	// Use Argon2id for key derivation (add golang.org/x/crypto/argon2)
	// salt := sha256.Sum256([]byte(roomName))
	// key := argon2.IDKey([]byte(password), salt[:], 1, 64*1024, 4, 32)

	// Placeholder - implement with proper KDF
	return &RoomCrypto{}, nil
}
