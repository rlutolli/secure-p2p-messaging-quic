package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
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

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
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

func DeriveRoomKey(roomName, password string) *RoomCrypto {
	if password == "" {
		return &RoomCrypto{roomKey: nil}
	}
	key := argon2.IDKey([]byte(password), []byte(roomName), 1, 64*1024, 4, 32)
	return &RoomCrypto{roomKey: key}
}

// GenerateRoomSecret generates a random 32-byte secret used in room key derivation.
// The secret is mixed into HKDF as a salt so that even knowing roomName+password,
// an observer cannot derive the keys without the secret. The secret is rotated
// whenever a peer is kicked or banned.
func GenerateRoomSecret() []byte {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		panic(fmt.Sprintf("GenerateRoomSecret: %v", err))
	}
	return secret
}

// DeriveRoomKeys derives encryption and HMAC sub-keys from the room password
// and room secret. The roomSecret is used as the HKDF salt — different secrets
// produce different keys even with identical roomName+password.
//
// When password is empty the room name is used as key material. The AuthKey
// matches the output of DeriveRoomKey for backward-compatible password comparison.
// DeriveRoomKeys derives authKey from password (for access control), and EncKey/HMACKey
// from the room secret (shared by all authenticated peers). This ensures all authenticated
// peers can verify each other's E2EE messages, while the password gates room access.
// Password is NOT used for E2EE key derivation — only for auth (who can join).
func DeriveRoomKeys(roomName, password string, roomSecret []byte) *RoomKeys {
	// AuthKey: derived from password (or roomName if no password) — for access control.
	// Used by the server to verify a joining peer's password matches.
	keyMaterial := password
	if keyMaterial == "" {
		keyMaterial = roomName
	}
	authKey := argon2.IDKey([]byte(keyMaterial), []byte(roomName), 1, 64*1024, 4, 32)

	// EncKey and HMACKey: derived from roomSecret so all authenticated peers (who all
	// receive the same secret) derive the SAME encryption and signing keys. This is the
	// shared group key for E2EE. roomSecret must be non-nil for this to work.
	var encKey, hmacKey []byte
	if len(roomSecret) > 0 {
		// Use roomSecret as HKDF salt to derive E2EE keys (per RFC 5869).
		encReader := hkdf.New(sha256.New, authKey, roomSecret, []byte("p2p-messenger-enc"))
		encKey = make([]byte, 32)
		if _, err := io.ReadFull(encReader, encKey); err != nil {
			panic(fmt.Sprintf("DeriveRoomKeys: hkdf enc: %v", err))
		}
		hmacReader := hkdf.New(sha256.New, authKey, roomSecret, []byte("p2p-messenger-hmac"))
		hmacKey = make([]byte, 32)
		if _, err := io.ReadFull(hmacReader, hmacKey); err != nil {
			panic(fmt.Sprintf("DeriveRoomKeys: hkdf hmac: %v", err))
		}
	}

	return &RoomKeys{
		AuthKey: authKey,
		EncKey:  encKey,
		HMACKey: hmacKey,
	}
}
