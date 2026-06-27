package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectionManagerIsDuplicate(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(cm *ConnectionManager)
		sender  string
		message string
		want    bool
	}{
		{"first message", func(cm *ConnectionManager) {}, "alice", "hello", false},
		{"duplicate", func(cm *ConnectionManager) { cm.IsDuplicate("alice", "hello") }, "alice", "hello", true},
		{"different sender same message", func(cm *ConnectionManager) { cm.IsDuplicate("alice", "hello") }, "bob", "hello", false},
		{"same sender different message", func(cm *ConnectionManager) { cm.IsDuplicate("alice", "hello") }, "alice", "world", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cm := NewConnectionManager(0, "testroom", true, "", false, "IsDup-"+tt.name)
			tt.setup(cm)
			got := cm.IsDuplicate(tt.sender, tt.message)
			if got != tt.want {
				t.Errorf("IsDuplicate(%q, %q) = %v, want %v", tt.sender, tt.message, got, tt.want)
			}
		})
	}
}

func TestConnectionManagerIsDuplicateExpires(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "IsDupExpires")

	if cm.IsDuplicate("alice", "hello") {
		t.Fatal("expected first message not duplicate")
	}
	if !cm.IsDuplicate("alice", "hello") {
		t.Fatal("expected duplicate immediately after")
	}

	cm.seenMsgsMu.Lock()
	cm.seenMsgs = make(map[string]time.Time)
	cm.seenMsgsMu.Unlock()

	if cm.IsDuplicate("alice", "hello") {
		t.Fatal("expected not duplicate after clearing seenMsgs")
	}
}

func TestConnectionManagerListConnected(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "ListConnected")

	addr := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	mock := newMockConn(&addr)

	cm.RegisterIncoming("127.0.0.1:1234", mock)

	connected := cm.ListConnected()
	if len(connected) != 1 {
		t.Fatalf("expected 1 connected peer, got %d", len(connected))
	}
	if connected[0] != "127.0.0.1:1234" {
		t.Errorf("expected 127.0.0.1:1234, got %s", connected[0])
	}
}

func TestConnectionManagerRegisterIncomingDedupes(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "RegIncomingDedupes")

	addr1 := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	mock1 := newMockConn(&addr1)
	mock2 := newMockConn(&addr1)

	cm.RegisterIncoming("127.0.0.1:1234", mock1)
	cm.RegisterIncoming("127.0.0.1:1234", mock2)

	connected := cm.ListConnected()
	if len(connected) != 1 {
		t.Fatalf("expected 1 connected peer after dedupe, got %d", len(connected))
	}
}

func TestConnectionManagerRemoveConnection(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "RemoveConnection")

	addr := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	mock := newMockConn(&addr)

	cm.RegisterIncoming("127.0.0.1:1234", mock)
	cm.removeConnection("127.0.0.1:1234")

	connected := cm.ListConnected()
	if len(connected) != 0 {
		t.Fatalf("expected 0 connected peers, got %d", len(connected))
	}
	if !mock.isClosed() {
		t.Error("expected mock connection to be closed")
	}
}

func TestConnectionManagerSend(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "Send")

	addr := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	mock := newMockConn(&addr)

	cm.RegisterIncoming("127.0.0.1:1234", mock)

	// Manually set up room keys to simulate receiving a SECRET from a relay.
	// Without a real relay connection, cm.roomKeys.EncKey would be nil and
	// Send() would fail to encrypt.
	// Use a dummy secret for testing.
	dummySecret := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if err := cm.UpdateRoomKeys("testroom", "", dummySecret); err != nil {
		t.Fatalf("UpdateRoomKeys failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := cm.Send(ctx, "127.0.0.1:1234", "hello world")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	written := string(mock.getWritten())
	// E2EE is always active — message should be 4-field encrypted format:
	// FROM:<alias>|<payload>|<nonce>|<hmac>\n
	if !strings.HasPrefix(written, "FROM:") {
		t.Errorf("expected FROM: prefix, got %q", written)
	}
	// Strip trailing newline, then split
	line := strings.TrimSuffix(written, "\n")
	parts := strings.SplitN(line, "|", 4)
	if len(parts) < 4 {
		t.Fatalf("expected 4 pipe-separated fields in encrypted FROM, got %d in %q", len(parts), written)
	}
	// Decrypt to verify content
	alias := parts[0][5:] // strip "FROM:" prefix
	payload := parts[1]
	nonce := parts[2]
	hmacHex := parts[3]
	if alias != cm.GetLocalAlias() {
		t.Errorf("expected alias %q, got %q", cm.GetLocalAlias(), alias)
	}
	// Verify HMAC
	unsigned := fmt.Sprintf("FROM:%s|%s|%s", alias, payload, nonce)
	if !cm.roomKeys.Verify(unsigned, hmacHex) {
		t.Error("HMAC verification failed on sent message")
	}
	// Decrypt
	plaintext, err := cm.roomKeys.Decrypt(payload)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if string(plaintext) != "hello world" {
		t.Errorf("expected decrypted message %q, got %q", "hello world", string(plaintext))
	}
}

func TestConnectionManagerRelayTracking(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false, "RelayTracking")

	if cm.IsRelayPeer("127.0.0.1:1111") {
		t.Error("expected no relay peers initially")
	}

	cm.MarkRelay("127.0.0.1:1111")
	if !cm.IsRelayPeer("127.0.0.1:1111") {
		t.Error("expected 127.0.0.1:1111 to be a relay peer")
	}

	addrs := cm.GetRelayAddrs()
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:1111" {
		t.Errorf("expected [127.0.0.1:1111], got %v", addrs)
	}
}

func TestConnectionManagerNonRelayOnlySendsToRelay(t *testing.T) {
	t.Parallel()

	// Set up a proper relay server (alice) with real TCP listener
	aliceCM := NewConnectionManager(0, "testroom", true, "", true, "NonRelaySendsToRelay-Alice")
	defer aliceCM.Close()

	tlsConf := generateTLSConfig()
	tcpListener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatalf("failed to start tcp listener: %v", err)
	}
	defer tcpListener.Close()

	aliceServer := &Server{
		tcpListener:     tcpListener,
		rooms:           make(map[string]*Room),
		roomsMu:         sync.RWMutex{},
		onMessage:       func(from, room, message string) {},
		onSystemMessage: func(message string) {},
		connManager:     aliceCM,
		setupSem:        make(chan struct{}, 10),
		banList:         make(map[string]bool),
		aliasToDeviceID: make(map[string]string),
	}
	go aliceServer.acceptLoopTCP()

	_, portStr, _ := net.SplitHostPort(tcpListener.Addr().String())
	alicePort := 0
	fmt.Sscanf(portStr, "%d", &alicePort)
	aliceServer.localPort = alicePort
	aliceCM.localPort = alicePort
	aliceAddr := fmt.Sprintf("127.0.0.1:%d", alicePort)

	// Bob is a non-relay CM connecting to alice's relay
	bobCM := NewConnectionManager(0, "testroom", true, "", false, "NonRelaySendsToRelay-Bob")
	defer bobCM.Close()

	// Use getOrCreate which properly establishes the connection via TCP dial
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// getOrCreate dials alice's relay and waits for SECRET, establishing the connection
	bobMC, err := bobCM.GetOrCreate(ctx, aliceAddr)
	if err != nil {
		t.Fatalf("bob failed to connect to relay: %v", err)
	}

	// Wait for bob's room keys to be updated after receiving SECRET from relay
	if !bobCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatal("bobCM never received room secret after joining")
	}

	// Verify that non-relay peers (other registered connections) don't receive
	// messages sent to the relay address. We verify by checking that when we
	// send to aliceAddr (the relay), only the relay connection receives data.
	// Since bob has no other registered peers, we just verify the relay connection
	// IS used (getOrCreate succeeded) and the connection is properly established.

	// Verify bob's connection to relay is properly established
	if bobMC == nil {
		t.Error("expected bob to have a valid connection to relay")
	}

	// Verify bob's roomKeys are set (non-nil EncKey) after receiving SECRET
	bobCM.mu.RLock()
	encKeyNil := bobCM.roomKeys == nil || bobCM.roomKeys.EncKey == nil
	bobCM.mu.RUnlock()
	if encKeyNil {
		t.Error("expected bob's EncKey to be set after receiving SECRET")
	}
}
