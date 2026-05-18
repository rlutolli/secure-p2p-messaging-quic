package main

import (
	"context"
	"net"
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
			cm := NewConnectionManager(0, "testroom", true, "", false)
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
	cm := NewConnectionManager(0, "testroom", true, "", false)

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
	cm := NewConnectionManager(0, "testroom", true, "", false)

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
	cm := NewConnectionManager(0, "testroom", true, "", false)

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
	cm := NewConnectionManager(0, "testroom", true, "", false)

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
	cm := NewConnectionManager(0, "testroom", true, "", false)

	addr := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	mock := newMockConn(&addr)

	cm.RegisterIncoming("127.0.0.1:1234", mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := cm.Send(ctx, "127.0.0.1:1234", "hello world")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	written := mock.getWritten()
	expected := "FROM:" + cm.GetLocalAlias() + "|hello world\n"
	if string(written) != expected {
		t.Errorf("expected %q, got %q", expected, string(written))
	}
}

func TestConnectionManagerRelayTracking(t *testing.T) {
	t.Parallel()
	cm := NewConnectionManager(0, "testroom", true, "", false)

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
	cm := NewConnectionManager(0, "testroom", true, "", false)

	addr1 := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111}
	mock1 := newMockConn(&addr1)
	cm.RegisterIncoming("127.0.0.1:1111", mock1)
	cm.MarkRelay("127.0.0.1:1111")

	addr2 := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}
	mock2 := newMockConn(&addr2)
	cm.RegisterIncoming("127.0.0.1:2222", mock2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	relays := cm.GetRelayAddrs()
	for _, addr := range relays {
		cm.Send(ctx, addr, "hello")
	}

	if len(mock1.getWritten()) == 0 {
		t.Error("expected relay peer to receive message")
	}
	if len(mock2.getWritten()) != 0 {
		t.Error("expected non-relay peer not to receive message")
	}
}
