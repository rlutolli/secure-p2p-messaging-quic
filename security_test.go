package main

import (
	"crypto/tls"
	"testing"
)

func TestCollectLocalIPs(t *testing.T) {
	t.Parallel()
	ips := collectLocalIPs()
	if len(ips) == 0 {
		t.Fatal("collectLocalIPs returned no IPs")
	}
	hasLoopback := false
	for _, ip := range ips {
		if ip.IsLoopback() {
			hasLoopback = true
			break
		}
	}
	if !hasLoopback {
		t.Error("expected at least one loopback IP")
	}
}

func TestDeriveRoomKey(t *testing.T) {
	t.Parallel()
	rc1 := DeriveRoomKey("testroom", "password")
	if rc1 == nil {
		t.Fatal("DeriveRoomKey returned nil RoomCrypto")
	}
	if len(rc1.roomKey) == 0 {
		t.Fatal("expected non-empty roomKey")
	}

	rc2 := DeriveRoomKey("testroom", "password")
	if string(rc1.roomKey) != string(rc2.roomKey) {
		t.Error("expected consistent key derivation")
	}

	rc3 := DeriveRoomKey("testroom", "different")
	if string(rc1.roomKey) == string(rc3.roomKey) {
		t.Error("expected different keys for different passwords")
	}

	rc4 := DeriveRoomKey("testroom", "")
	if rc4.roomKey != nil {
		t.Error("expected nil roomKey for empty password")
	}
}

func TestGenerateTLSConfig(t *testing.T) {
	t.Parallel()
	cfg := generateTLSConfig()
	if cfg == nil {
		t.Fatal("generateTLSConfig returned nil")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected MinVersion TLS 1.3, got %d", cfg.MinVersion)
	}
	if len(cfg.Certificates) == 0 {
		t.Error("expected at least one certificate")
	}
}
