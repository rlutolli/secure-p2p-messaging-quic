package main

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestCM() *ConnectionManager {
	return NewConnectionManager(0, "test-room", false, "", false, "TofuCM")
}

// First contact pins; same fingerprint later is accepted; a different
// fingerprint for the same address is rejected as a possible MITM.
func TestTOFU_PinAcceptReject(t *testing.T) {
	cm := newTestCM()
	addr := "1.2.3.4:9000"
	fpA := "aaaa1111"
	fpB := "bbbb2222"

	if err := cm.checkAndPin(addr, fpA); err != nil {
		t.Fatalf("first contact should pin and accept, got %v", err)
	}
	if err := cm.checkAndPin(addr, fpA); err != nil {
		t.Fatalf("same fingerprint should be accepted, got %v", err)
	}
	if err := cm.checkAndPin(addr, fpB); err == nil {
		t.Fatalf("changed fingerprint must be rejected (possible MITM), got nil")
	}
}

// A pre-populated known-peers file is loaded, so first contact is verified
// against the supplied fingerprint instead of trusted on sight: a mismatching
// cert on the very first connection is rejected.
func TestTOFU_PrePopulatedKnownPeersRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_peers")
	addr := "10.0.0.1:41748"
	pinned := "deadbeefcafe"
	if err := os.WriteFile(path, []byte("# out-of-band pin\n"+addr+" "+pinned+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cm := newTestCM()
	if err := cm.LoadKnownPeers(path); err != nil {
		t.Fatalf("LoadKnownPeers: %v", err)
	}
	if err := cm.checkAndPin(addr, "0000ffff"); err == nil {
		t.Fatalf("first contact with a cert that does not match the pre-pinned fingerprint must be rejected")
	}
	if err := cm.checkAndPin(addr, pinned); err != nil {
		t.Fatalf("first contact matching the pre-pinned fingerprint must be accepted, got %v", err)
	}
}

// A newly observed pin is persisted, and a fresh manager loading that store
// then rejects a different fingerprint — i.e. pins survive a restart.
func TestTOFU_PersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_peers")
	addr := "192.168.1.5:41748"
	fp := "1234abcd5678"

	cm1 := newTestCM()
	if err := cm1.LoadKnownPeers(path); err != nil { // sets path, file absent is OK
		t.Fatalf("LoadKnownPeers (new store): %v", err)
	}
	if err := cm1.checkAndPin(addr, fp); err != nil {
		t.Fatalf("first contact should pin+persist, got %v", err)
	}

	// Simulate a restart: a brand-new manager that loads the same store.
	cm2 := newTestCM()
	if err := cm2.LoadKnownPeers(path); err != nil {
		t.Fatalf("LoadKnownPeers (existing store): %v", err)
	}
	if err := cm2.checkAndPin(addr, "ffff0000"); err == nil {
		t.Fatalf("after restart, a changed fingerprint must be rejected from the persisted pin")
	}
	if err := cm2.checkAndPin(addr, fp); err != nil {
		t.Fatalf("after restart, the persisted fingerprint must still be accepted, got %v", err)
	}
}
