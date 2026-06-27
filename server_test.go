package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func newTestServer() *Server {
	return &Server{
		rooms:           make(map[string]*Room),
		onMessage:       func(from, room, message string) {},
		onSystemMessage: func(message string) {},
		connManager:     NewConnectionManager(0, "testroom", true, "", false, "TestPeer"),
		banList:         make(map[string]bool),
		aliasToDeviceID: make(map[string]string),
	}
}

func TestHandleMessageJoin(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	mock := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer := &Peer{addr: "127.0.0.1:1111", alias: "TestPeer", conn: mock}

	s.handleMessage(peer, "JOIN:testroom|MyAlias")
	if peer.room == nil {
		t.Fatal("expected peer to be in a room")
	}
	if peer.room.name != "testroom" {
		t.Errorf("expected room testroom, got %s", peer.room.name)
	}
	if peer.alias != "MyAlias" {
		t.Errorf("expected alias MyAlias, got %s", peer.alias)
	}
}

func TestHandleMessagePing(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	mock := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer := &Peer{addr: "127.0.0.1:1111", alias: "TestPeer", conn: mock}

	s.handleMessage(peer, "PING")
	written := mock.getWritten()
	if string(written) != "PONG\n" {
		t.Errorf("expected PONG\\n, got %q", string(written))
	}
}

func TestHandleMessagePong(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	mock := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer := &Peer{addr: "127.0.0.1:1111", alias: "TestPeer", conn: mock}

	s.handleMessage(peer, "PONG")
	written := mock.getWritten()
	if len(written) != 0 {
		t.Errorf("expected no write for PONG, got %q", string(written))
	}
}

func TestHandleMessageFrom(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	keys := DeriveRoomKeys("testroom", "", nil)
	s.joinRoom(peer1, "testroom", keys, false, nil)

	// Drain SECRET sent to alice (key rotation triggered by first join).
	mock1.drainSECRET()

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.joinRoom(peer2, "testroom", keys, false, nil)

	// Drain any pending SECRET messages.
	time.Sleep(200 * time.Millisecond)
	mock1.drainSECRET()
	mock2.drainSECRET()

	// IMPORTANT: Use the room's keys for sending, not the original 'keys' derived
	// with nil secret. The room re-derives keys with room.roomSecret, so to get
	// a matching HMAC we must use s.rooms["testroom"].keys.
	roomKeys := s.rooms["testroom"].keys
	payload, _ := roomKeys.Encrypt([]byte("hello"))
	nonce := generateReplayNonce()
	unsigned := fmt.Sprintf("FROM:Alice|%s|%s", payload, nonce)
	sig := roomKeys.Sign(unsigned)
	encryptedMsg := unsigned + "|" + sig

	s.handleMessage(peer1, encryptedMsg)
	time.Sleep(100 * time.Millisecond)

	written := mock2.getWritten()
	// The relay forwards the encrypted payload verbatim.
	expectedPrefix := "FROM:Alice|"
	if !strings.Contains(string(written), expectedPrefix) {
		t.Errorf("expected broadcast to contain %q prefix, got %q", expectedPrefix, string(written))
	}
}

func TestHandleMessageMSG(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.joinRoom(peer1, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	// Drain SECRET sent to alice after first join.
	mock1.drainSECRET()

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.joinRoom(peer2, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	// Drain SECRET sent to alice and bob after bob joins.
	time.Sleep(200 * time.Millisecond)
	mock1.drainSECRET()
	mock2.drainSECRET()

	s.handleMessage(peer1, "MSG:hello world")
	time.Sleep(100 * time.Millisecond)

	written := mock2.getWritten()
	expected := "FROM:Alice|hello world\n"
	if string(written) != expected {
		t.Errorf("expected %q, got %q", expected, string(written))
	}
}

func TestJoinRoom(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	mock := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock}

	s.joinRoom(peer, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	s.roomsMu.RLock()
	room, exists := s.rooms["testroom"]
	s.roomsMu.RUnlock()
	if !exists {
		t.Fatal("expected room to exist")
	}

	room.peersMu.RLock()
	p, exists := room.peers["127.0.0.1:1111"]
	room.peersMu.RUnlock()
	if !exists {
		t.Fatal("expected peer to be in room")
	}
	if p != peer {
		t.Error("peer mismatch")
	}
}

func TestBroadcastToRoom(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.joinRoom(peer1, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.joinRoom(peer2, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	s.broadcastToRoom(peer1.room, peer1.addr, "hello")
	time.Sleep(100 * time.Millisecond)

	written := mock2.getWritten()
	expected := "FROM:Alice|hello\n"
	if !strings.Contains(string(written), expected) {
		t.Errorf("expected %q in peer2 writes, got %q", expected, string(written))
	}

	if strings.Contains(string(mock1.getWritten()), expected) {
		t.Errorf("expected sender not to receive message %q, got %q", expected, string(mock1.getWritten()))
	}
}

func TestRemovePeer(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.joinRoom(peer1, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.joinRoom(peer2, "testroom", DeriveRoomKeys("testroom", "", nil), false, nil)

	s.removePeer(peer1)
	time.Sleep(100 * time.Millisecond)

	s.roomsMu.RLock()
	_, exists := s.rooms["testroom"]
	s.roomsMu.RUnlock()
	if !exists {
		t.Error("expected room to still exist")
	}

	s.rooms["testroom"].peersMu.RLock()
	_, exists = s.rooms["testroom"].peers["127.0.0.1:1111"]
	s.rooms["testroom"].peersMu.RUnlock()
	if exists {
		t.Error("expected peer1 to be removed from room")
	}

	if !mock1.isClosed() {
		t.Error("expected peer1 connection to be closed")
	}

	written := mock2.getWritten()
	if !strings.Contains(string(written), "left the chat") {
		t.Errorf("expected leave message to be broadcast, got %q", string(written))
	}
}

func TestCreateRoomWithPassword(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	mock := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock}

	s.handleMessage(peer, "JOIN:pwroom|Alice|mypassword")

	if peer.room == nil {
		t.Fatal("expected peer to be in a room")
	}
	if peer.room.name != "pwroom" {
		t.Errorf("expected room pwroom, got %s", peer.room.name)
	}
	if peer.room.keys == nil {
		t.Error("expected room to have a password hash set")
	}
}

func TestJoinRoomWithPassword(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.handleMessage(peer1, "JOIN:secureroom|Alice|secret123")

	if peer1.room == nil {
		t.Fatal("expected peer1 to be in a room")
	}
	if peer1.room.name != "secureroom" {
		t.Errorf("expected room secureroom, got %s", peer1.room.name)
	}
	if peer1.room.keys == nil {
		t.Error("expected room to have a password hash")
	}

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.handleMessage(peer2, "JOIN:secureroom|Bob|secret123")

	if peer2.room == nil {
		t.Fatal("expected peer2 to join the room")
	}
	if peer2.room.name != "secureroom" {
		t.Errorf("expected room secureroom, got %s", peer2.room.name)
	}
}

func TestJoinRoomWithWrongPassword(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.handleMessage(peer1, "JOIN:secureroom|Alice|secret123")

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.handleMessage(peer2, "JOIN:secureroom|Bob|wrongpassword")

	if peer2.room != nil {
		t.Error("expected peer2 to be rejected from room")
	}

	written := mock2.getWritten()
	if !strings.Contains(string(written), "AUTH:FAILED") {
		t.Errorf("expected AUTH:FAILED response, got %q", string(written))
	}
}

func TestBroadcastToMultiplePeers(t *testing.T) {
	t.Parallel()
	s := newTestServer()

	keys := DeriveRoomKeys("testroom", "", nil)

	mock1 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1111})
	peer1 := &Peer{addr: "127.0.0.1:1111", alias: "Alice", conn: mock1}
	s.joinRoom(peer1, "testroom", keys, false, nil)

	mock2 := newMockConn(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222})
	peer2 := &Peer{addr: "127.0.0.1:2222", alias: "Bob", conn: mock2}
	s.joinRoom(peer2, "testroom", keys, false, nil)

	// IMPORTANT: For unauthenticated rooms, the room's keys are re-derived with
	// room.roomSecret (not nil). So we must use s.rooms["testroom"].keys when
	// encrypting alice's message, NOT the original 'keys' derived with nil secret.
	// Otherwise HMAC verification fails because authKeys don't match.
	roomKeys := s.rooms["testroom"].keys
	payload, _ := roomKeys.Encrypt([]byte("hello relay"))
	nonce := generateReplayNonce()
	unsigned := fmt.Sprintf("FROM:Alice|%s|%s", payload, nonce)
	sig := roomKeys.Sign(unsigned)
	encryptedMsg := unsigned + "|" + sig

	s.handleMessage(peer1, encryptedMsg)
	time.Sleep(100 * time.Millisecond)

	written := mock2.getWritten()
	if !strings.Contains(string(written), "FROM:Alice|") {
		t.Errorf("expected encrypted FROM:Alice| in peer2 writes, got %q", string(written))
	}
}
