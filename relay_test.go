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
	mc := &ManagedConnection{
		conn:      cc,
		peerAddr:  relayAddr,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}

	cm.mu.Lock()
	cm.connections[relayAddr] = mc
	cm.mu.Unlock()
	go cm.readLoop(mc)

	return cc
}

func TestRelayStarTopology(t *testing.T) {
	aliceCM := NewConnectionManager(0, "testroom", true, "", true)
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
		onMessage:       func(from, room, message string) {},
		onSystemMessage: func(message string) {},
		connManager:     aliceCM,
	}
	go aliceServer.acceptLoopTCP()

	_, portStr, _ := net.SplitHostPort(tcpListener.Addr().String())
	alicePort := 0
	fmt.Sscanf(portStr, "%d", &alicePort)
	aliceServer.localPort = alicePort
	aliceCM.localPort = alicePort

	aliceAddr := fmt.Sprintf("127.0.0.1:%d", alicePort)

	bobCM := NewConnectionManager(0, "testroom", true, "", false)
	defer bobCM.Close()
	bobConn := connectPeerToRelay(t, bobCM, aliceAddr)
	bobCM.MarkRelay(aliceAddr)

	time.Sleep(200 * time.Millisecond)

	charlieCM := NewConnectionManager(0, "testroom", true, "", false)
	defer charlieCM.Close()
	charlieConn := connectPeerToRelay(t, charlieCM, aliceAddr)
	charlieCM.MarkRelay(aliceAddr)

	time.Sleep(200 * time.Millisecond)

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
	err = bobCM.Send(ctx, aliceAddr, "hello")
	cancel()
	if err != nil {
		t.Fatalf("bob send failed: %v", err)
	}

	sent := bobConn.getWrites()
	if !strings.Contains(string(sent), "FROM:"+bobCM.GetLocalAlias()+"|hello") {
		t.Fatalf("expected bob to send hello, got %q", string(sent))
	}

	var charlieReceived bool
	expectedMsg := "FROM:" + bobCM.GetLocalAlias() + "|hello"
	for i := 0; i < 50; i++ {
		if strings.Contains(string(charlieConn.getReads()), expectedMsg) {
			charlieReceived = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !charlieReceived {
		t.Fatalf("expected charlie to receive hello from bob, got %q", string(charlieConn.getReads()))
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
