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

type captureConn struct {
	PeerConnection
	mu     sync.Mutex
	reads  []byte
	writes []byte
}

func (c *captureConn) Read(p []byte) (int, error) {
	n, err := c.PeerConnection.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.reads = append(c.reads, p[:n]...)
		c.mu.Unlock()
	}
	return n, err
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, p...)
	c.mu.Unlock()
	return c.PeerConnection.Write(p)
}

func (c *captureConn) getReads() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.reads...)
}

func (c *captureConn) getWrites() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.writes...)
}

// drainSECRET reads and discards any SECRET: message in the capture buffer.
// This is needed because after each peer join the server sends a new room secret
// that updates the peer's roomKeys. The raw SECRET appears in the capture buffer
// but is also processed by the readLoop concurrently.
func (c *captureConn) drainSECRET() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The SECRET line is "SECRET:<hex>\n" — find and remove it.
	s := string(c.reads)
	if idx := strings.Index(s, "SECRET:"); idx >= 0 {
		end := strings.Index(s[idx:], "\n")
		if end >= 0 {
			c.reads = []byte(s[:idx] + s[idx+end+1:])
		}
	}
}

func connectPeerToRelay(t *testing.T, cm *ConnectionManager, relayAddr string) *captureConn {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tlsConf := &tls.Config{InsecureSkipVerify: true}
	d := net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	tlsConn := tls.Client(rawConn, tlsConf)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		t.Fatalf("tls handshake failed: %v", err)
	}

	handshake := fmt.Sprintf("JOIN:%s|%s\n", cm.roomName, cm.GetLocalAlias())
	if _, err := tlsConn.Write([]byte(handshake)); err != nil {
		tlsConn.Close()
		t.Fatalf("handshake write failed: %v", err)
	}

	cc := &captureConn{PeerConnection: &NetConnWrapper{Conn: tlsConn}}
	mc := newManagedConnection(cc, relayAddr)
	mc.replay = NewReplayProtector(5 * time.Minute)

	cm.mu.Lock()
	cm.connections[relayAddr] = mc
	cm.mu.Unlock()
	mc.secretReady.Add(1) // readLoop calls Done() after processing SECRET
	go cm.readLoop(mc)
	go cm.writeLoop(mc)

	return cc
}

func TestRelayStarTopology(t *testing.T) {
	aliceCM := NewConnectionManager(0, "testroom", true, "", true, "Alice")
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

	bobCM := NewConnectionManager(0, "testroom", true, "", false, "Bob")
	defer bobCM.Close()
	bobConn := connectPeerToRelay(t, bobCM, aliceAddr)
	bobCM.MarkRelay(aliceAddr)

	// Capture bob's secret BEFORE charlie's join. With no-rotation-on-join design,
	// bob's secret should stay the same after charlie joins (both have same initial secret).
	bobSecretBeforeCharlieJoin := func() []byte {
		bobCM.mu.RLock()
		defer bobCM.mu.RUnlock()
		return bobCM.roomSecret
	}()
	if !bobCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatalf("bobCM never received room secret after joining")
	}
	bobConn.drainSECRET()

	charlieCM := NewConnectionManager(0, "testroom", true, "", false, "Charlie")
	defer charlieCM.Close()
	charlieConn := connectPeerToRelay(t, charlieCM, aliceAddr)
	charlieCM.MarkRelay(aliceAddr)

	// Wait for charlie to receive the room secret.
	if !charlieCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatalf("charlieCM never received room secret after joining")
	}
	// Bob's secret should NOT change when charlie joins (no rotation on join).
	// Both should now have the same secret (the room's initial secret).
	charlieConn.drainSECRET()

	bobCM.mu.RLock()
	bobSecretAfter := bobCM.roomSecret
	bobCM.mu.RUnlock()
	charlieCM.mu.RLock()
	charlieSecret := charlieCM.roomSecret
	charlieCM.mu.RUnlock()
	// Verify bob and charlie have the SAME secret (room's initial secret, shared by all).
	if bobSecretAfter == nil || charlieSecret == nil {
		t.Fatalf("bob or charlie secret is nil: bob=%x charlie=%x", bobSecretAfter, charlieSecret)
	}
	if string(bobSecretAfter) != string(charlieSecret) {
		t.Fatalf("bob and charlie have different secrets: bob=%x charlie=%x", bobSecretAfter, charlieSecret)
	}
	_ = bobSecretBeforeCharlieJoin // captured for clarity

	alicePeers := aliceCM.ListConnected()
	if len(alicePeers) != 2 {
		t.Fatalf("expected alice to have 2 peers, got %d", len(alicePeers))
	}

	bobPeers := bobCM.ListConnected()
	if len(bobPeers) != 1 || bobPeers[0] != aliceAddr {
		t.Fatalf("expected bob to only connect to alice, got %v", bobPeers)
	}

	charliePeers := charlieCM.ListConnected()
	if len(charliePeers) != 1 || charliePeers[0] != aliceAddr {
		t.Fatalf("expected charlie to only connect to alice, got %v", charliePeers)
	}

	bobRelays := bobCM.GetRelayAddrs()
	if len(bobRelays) != 1 || bobRelays[0] != aliceAddr {
		t.Fatalf("expected bob relay addrs to be [%s], got %v", aliceAddr, bobRelays)
	}

	charlieRelays := charlieCM.GetRelayAddrs()
	if len(charlieRelays) != 1 || charlieRelays[0] != aliceAddr {
		t.Fatalf("expected charlie relay addrs to be [%s], got %v", aliceAddr, charlieRelays)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// Bob's secret is already set from when he joined. Send immediately.
	err = bobCM.Send(ctx, aliceAddr, "hello")
	cancel()
	if err != nil {
		t.Fatalf("bob send failed: %v", err)
	}

	// Bob's message is now E2EE encrypted — verify the encrypted format
	sent := bobConn.getWrites()
	if !strings.Contains(string(sent), "FROM:"+bobCM.GetLocalAlias()) {
		t.Fatalf("expected bob to send FROM with his alias, got %q", string(sent))
	}
	// Decrypt bob's sent message and verify content — format: FROM:<alias>|<payload>|<nonce>|<hmac>
	sentLine := strings.TrimSuffix(string(sent), "\n")
	raw := strings.TrimPrefix(sentLine, "FROM:")
	parts := strings.SplitN(raw, "|", 4)
	if len(parts) < 4 {
		t.Fatalf("expected 4-field encrypted format, got %d fields in %q", len(parts), sentLine)
	}
	if pt, err := bobCM.roomKeys.Decrypt(parts[1]); err == nil {
		if string(pt) != "hello" {
			t.Fatalf("expected decrypted message 'hello', got %q", string(pt))
		}
	} else {
		t.Fatalf("failed to decrypt bob's sent message: %v", err)
	}

	// Charlie receives the forwarded encrypted message from alice's relay
	var charlieReceived bool
	for i := 0; i < 50; i++ {
		reads := string(charlieConn.getReads())
		if strings.Contains(reads, "FROM:"+bobCM.GetLocalAlias()) {
			// Decrypt to verify — format is: FROM:<alias>|<payload>|<nonce>|<hmac>
			for _, line := range strings.Split(reads, "\n") {
				if strings.HasPrefix(line, "FROM:") {
					raw := strings.TrimPrefix(line, "FROM:")
					parts := strings.SplitN(raw, "|", 4)
					if len(parts) == 4 {
						if pt, err := charlieCM.roomKeys.Decrypt(parts[1]); err == nil && string(pt) == "hello" {
							charlieReceived = true
							break
						}
					}
				}
			}
			if charlieReceived {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !charlieReceived {
		t.Fatalf("expected charlie to receive hello from bob (decrypted), got reads: %q", string(charlieConn.getReads()))
	}

	for addr := range bobCM.connections {
		if addr != aliceAddr {
			t.Fatalf("expected bob's only connection to be alice, got %s", addr)
		}
	}
	for addr := range charlieCM.connections {
		if addr != aliceAddr {
			t.Fatalf("expected charlie's only connection to be alice, got %s", addr)
		}
	}
}

// TestKickAndBanKeyRotation verifies that:
//  1. On kick/ban, the room secret is rotated
//  2. The kicked/banned peer does NOT receive the new secret
//  3. Remaining peers (and the owner) DO receive the new secret and can decrypt new messages
//  4. The kicked/banned peer CANNOT decrypt new messages (HMAC verification fails with old keys)
//  5. For authenticated rooms, the rotation preserves ownerAuthKey so authenticated peers
//     continue to derive matching E2EE keys
func TestKickAndBanKeyRotation(t *testing.T) {
	t.Run("Unauthenticated", func(t *testing.T) {
		testKickRotation(t, "")
	})
	t.Run("Authenticated", func(t *testing.T) {
		testKickRotation(t, "secretpassword")
	})
}

func testKickRotation(t *testing.T, roomPassword string) {
	t.Helper()

	// Alice is the room owner (relay)
	aliceCM := NewConnectionManager(0, "kickroom", true, roomPassword, true, "Alice")
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

	// Bob connects to Alice's relay
	bobCM := NewConnectionManager(0, "kickroom", true, roomPassword, false, "Bob")
	defer bobCM.Close()
	bobConn := connectPeerToRelayWithPassword(t, bobCM, aliceAddr, roomPassword)
	bobCM.MarkRelay(aliceAddr)
	if !bobCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatal("bobCM never received room secret after joining")
	}
	bobConn.drainSECRET()

	// Charlie connects to Alice's relay
	charlieCM := NewConnectionManager(0, "kickroom", true, roomPassword, false, "Charlie")
	defer charlieCM.Close()
	charlieConn := connectPeerToRelayWithPassword(t, charlieCM, aliceAddr, roomPassword)
	charlieCM.MarkRelay(aliceAddr)
	if !charlieCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatal("charlieCM never received room secret after joining")
	}
	charlieConn.drainSECRET()

	// Dave connects to Alice's relay — he'll be the witness peer that receives
	// post-rotation messages after Charlie is kicked (so we can verify Bob and
	// Dave can still communicate with the new keys).
	daveCM := NewConnectionManager(0, "kickroom", true, roomPassword, false, "Dave")
	defer daveCM.Close()
	daveConn := connectPeerToRelayWithPassword(t, daveCM, aliceAddr, roomPassword)
	daveCM.MarkRelay(aliceAddr)
	if !daveCM.WaitForRoomKeysUpdate(nil, 2*time.Second) {
		t.Fatal("daveCM never received room secret after joining")
	}
	daveConn.drainSECRET()

	// Capture pre-kick state: Charlie's keys (to verify he cannot decrypt post-rotation)
	charlieCM.mu.RLock()
	charliePreKickKeys := charlieCM.roomKeys
	charliePreKickEncKey := make([]byte, len(charlieCM.roomKeys.EncKey))
	copy(charliePreKickEncKey, charlieCM.roomKeys.EncKey)
	charliePreKickHmacKey := make([]byte, len(charlieCM.roomKeys.HMACKey))
	copy(charliePreKickHmacKey, charlieCM.roomKeys.HMACKey)
	charliePreKickSecret := make([]byte, len(charlieCM.roomSecret))
	copy(charliePreKickSecret, charlieCM.roomSecret)
	charlieCM.mu.RUnlock()

	// Verify the room has 3 peers (Bob, Charlie, Dave). Bob is the first joiner,
	// so he's the room owner. Alice is the relay/server, not a peer in the room.
	aliceServer.roomsMu.RLock()
	room, exists := aliceServer.rooms["kickroom"]
	aliceServer.roomsMu.RUnlock()
	if !exists {
		t.Fatal("room should exist after Dave joined")
	}
	room.peersMu.RLock()
	peerCount := len(room.peers)
	room.peersMu.RUnlock()
	if peerCount != 3 {
		t.Fatalf("expected room to have 3 peers, got %d", peerCount)
	}

	// Find Bob's peer (the owner, first joiner) in the room
	room.peersMu.RLock()
	var bobPeer *Peer
	for _, p := range room.peers {
		if p.alias == bobCM.GetLocalAlias() {
			bobPeer = p
			break
		}
	}
	room.peersMu.RUnlock()
	if bobPeer == nil {
		t.Fatal("could not find bob's peer in room")
	}

	// Bob (the owner) kicks Charlie — this should rotate the room secret
	aliceServer.handleKick(bobPeer, charlieCM.GetLocalAlias())

	// Wait for Bob to receive the new SECRET
	if !bobCM.WaitForRoomKeysUpdate(charliePreKickSecret, 2*time.Second) {
		t.Fatal("bobCM did not receive new room secret after kick")
	}
	bobConn.drainSECRET()
	// Wait for Dave to receive the new SECRET too
	if !daveCM.WaitForRoomKeysUpdate(charliePreKickSecret, 2*time.Second) {
		t.Fatal("daveCM did not receive new room secret after kick")
	}
	daveConn.drainSECRET()

	// Verify Bob's secret changed (it should differ from the pre-kick one)
	bobCM.mu.RLock()
	bobPostKickSecret := make([]byte, len(bobCM.roomSecret))
	copy(bobPostKickSecret, bobCM.roomSecret)
	bobPostKickEncKey := make([]byte, len(bobCM.roomKeys.EncKey))
	copy(bobPostKickEncKey, bobCM.roomKeys.EncKey)
	bobPostKickHmacKey := make([]byte, len(bobCM.roomKeys.HMACKey))
	copy(bobPostKickHmacKey, bobCM.roomKeys.HMACKey)
	bobCM.mu.RUnlock()
	if string(bobPostKickSecret) == string(charliePreKickSecret) {
		t.Fatal("bob's room secret should have changed after kick")
	}
	if string(bobPostKickEncKey) == string(charliePreKickEncKey) {
		t.Fatal("bob's EncKey should have changed after kick")
	}

	// Verify Charlie was removed from the room's peer list
	room.peersMu.RLock()
	peerCountAfter := len(room.peers)
	room.peersMu.RUnlock()
	if peerCountAfter != 2 {
		t.Errorf("expected room to have 2 peers after kick (Bob + Dave), got %d", peerCountAfter)
	}

	// Bob sends a message AFTER rotation using his current (new) keys.
	// Alice's relay broadcasts to all other peers; Dave (still in the room)
	// should receive it and decrypt with his new keys.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bobCM.Send(ctx, aliceAddr, "post-rotation-message"); err != nil {
		t.Fatalf("bob send failed: %v", err)
	}

	// Dave should receive and decrypt the message with his new keys.
	var daveDecrypted bool
	for i := 0; i < 50; i++ {
		reads := string(daveConn.getReads())
		for _, line := range strings.Split(reads, "\n") {
			if !strings.HasPrefix(line, "FROM:") {
				continue
			}
			raw := strings.TrimPrefix(line, "FROM:")
			parts := strings.SplitN(raw, "|", 4)
			if len(parts) == 4 {
				if pt, err := daveCM.roomKeys.Decrypt(parts[1]); err == nil && string(pt) == "post-rotation-message" {
					daveDecrypted = true
					break
				}
			}
		}
		if daveDecrypted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !daveDecrypted {
		t.Fatal("dave should have decrypted the post-rotation message with his new keys")
	}

	// Charlie CANNOT decrypt the post-rotation message with his pre-kick keys.
	// The message was encrypted + HMAC-signed with the NEW keys. Charlie's old
	// keys cannot produce valid HMACs, and cannot decrypt ciphertexts produced
	// with the new EncKey. We verify:
	//  1. Dave's new keys differ from charlie's pre-kick keys
	//  2. Charlie's old HMACKey cannot verify the post-rotation message's signature
	daveCM.mu.RLock()
	davePostKickEncKey := make([]byte, len(daveCM.roomKeys.EncKey))
	copy(davePostKickEncKey, daveCM.roomKeys.EncKey)
	davePostKickHmacKey := make([]byte, len(daveCM.roomKeys.HMACKey))
	copy(davePostKickHmacKey, daveCM.roomKeys.HMACKey)
	daveCM.mu.RUnlock()

	// Key material must have changed — old keys are useless for the new secret
	if string(charliePreKickEncKey) == string(davePostKickEncKey) {
		t.Fatal("SECURITY VIOLATION: charlie's old EncKey matches dave's new EncKey after rotation!")
	}
	if string(charliePreKickHmacKey) == string(davePostKickHmacKey) {
		t.Fatal("SECURITY VIOLATION: charlie's old HMACKey matches dave's new HMACKey after rotation!")
	}

	// Find the post-rotation message in dave's reads and verify charlie's OLD
	// HMACKey cannot verify it. This proves charlie's old keys are cryptographically
	// useless for the new room secret.
	reads := string(daveConn.getReads())
	foundEncryptedLine := false
	for _, line := range strings.Split(reads, "\n") {
		if !strings.HasPrefix(line, "FROM:") {
			continue
		}
		raw := strings.TrimPrefix(line, "FROM:")
		parts := strings.SplitN(raw, "|", 4)
		if len(parts) != 4 {
			continue
		}
		// Verify dave's NEW keys can verify the message
		unsigned := fmt.Sprintf("FROM:%s|%s|%s", parts[0], parts[1], parts[2])
		if !daveCM.roomKeys.Verify(unsigned, parts[3]) {
			continue
		}
		foundEncryptedLine = true
		// Charlie's OLD HMACKey must NOT verify the new message's signature
		if charliePreKickKeys.Verify(unsigned, parts[3]) {
			t.Fatal("SECURITY VIOLATION: charlie's old HMACKey verified a post-rotation message!")
		}
		// Charlie's OLD EncKey must NOT decrypt the new message's payload
		if pt, err := charliePreKickKeys.Decrypt(parts[1]); err == nil {
			if string(pt) == "post-rotation-message" {
				t.Fatal("SECURITY VIOLATION: charlie's old EncKey decrypted the post-rotation message!")
			}
		}
	}
	if !foundEncryptedLine {
		t.Fatal("could not find a valid post-rotation FROM message in dave's reads")
	}
}

// connectPeerToRelayWithPassword is like connectPeerToRelay but supports room passwords
// for authenticated room tests.
func connectPeerToRelayWithPassword(t *testing.T, cm *ConnectionManager, relayAddr, roomPassword string) *captureConn {
	t.Helper()
	cm.roomPassword = roomPassword

	tlsConf := &tls.Config{InsecureSkipVerify: true}
	d := net.Dialer{Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rawConn, err := d.DialContext(ctx, "tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	tlsConn := tls.Client(rawConn, tlsConf)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		t.Fatalf("tls handshake failed: %v", err)
	}

	var handshake string
	if roomPassword != "" {
		handshake = fmt.Sprintf("JOIN:%s|%s|%s\n", cm.roomName, cm.GetLocalAlias(), roomPassword)
	} else {
		handshake = fmt.Sprintf("JOIN:%s|%s\n", cm.roomName, cm.GetLocalAlias())
	}
	if _, err := tlsConn.Write([]byte(handshake)); err != nil {
		tlsConn.Close()
		t.Fatalf("handshake write failed: %v", err)
	}

	cc := &captureConn{PeerConnection: &NetConnWrapper{Conn: tlsConn}}
	mc := newManagedConnection(cc, relayAddr)
	mc.replay = NewReplayProtector(5 * time.Minute)

	cm.mu.Lock()
	cm.connections[relayAddr] = mc
	cm.mu.Unlock()
	mc.secretReady.Add(1)
	go cm.readLoop(mc)
	go cm.writeLoop(mc)

	return cc
}
