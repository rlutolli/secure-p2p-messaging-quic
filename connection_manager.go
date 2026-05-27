/*
connection_manager.go - Outgoing Connection Pool and Message Deduplication
*/
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type ConnectionManager struct {
	mu           sync.RWMutex
	connections  map[string]*ManagedConnection
	localPort    int
	roomName     string
	localAlias   string
	useTCP       bool
	roomPassword string

	sessionCache tls.ClientSessionCache

	seenMsgsMu sync.Mutex
	seenMsgs   map[string]time.Time

	// TOFU: stores the SHA-256 fingerprint of each peer's certificate on first connect.
	// Reconnections that present a different certificate are rejected.
	tofuMu           sync.RWMutex
	tofuFingerprints map[string]string

	pendingPingsMu sync.Mutex
	pendingPings   map[string]time.Time

	relayAddrs map[string]bool
	relayMu    sync.RWMutex
}

type ManagedConnection struct {
	conn      PeerConnection
	peerAddr  string
	createdAt time.Time
	lastUsed  time.Time
	mu        sync.Mutex
}

func NewConnectionManager(localPort int, roomName string, useTCP bool, roomPassword string, isRelay bool) *ConnectionManager {
	cm := &ConnectionManager{
		connections:      make(map[string]*ManagedConnection),
		localPort:        localPort,
		roomName:         roomName,
		localAlias:       generateAlias(),
		seenMsgs:         make(map[string]time.Time),
		useTCP:           useTCP,
		roomPassword:     roomPassword,
		sessionCache:     tls.NewLRUClientSessionCache(100),
		tofuFingerprints: make(map[string]string),
		pendingPings:     make(map[string]time.Time),
		relayAddrs:       make(map[string]bool),
	}

	go cm.cleanupSeenMsgs()
	go cm.healthCheck()

	return cm
}

func (cm *ConnectionManager) IsDuplicate(sender, message string) bool {
	hash := sha256.Sum256([]byte(sender + "|" + message))
	hashStr := hex.EncodeToString(hash[:8])

	cm.seenMsgsMu.Lock()
	defer cm.seenMsgsMu.Unlock()

	if _, seen := cm.seenMsgs[hashStr]; seen {
		return true
	}

	cm.seenMsgs[hashStr] = time.Now()
	return false
}

func (cm *ConnectionManager) cleanupSeenMsgs() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cm.seenMsgsMu.Lock()
		cutoff := time.Now().Add(-60 * time.Second)
		for hash, ts := range cm.seenMsgs {
			if ts.Before(cutoff) {
				delete(cm.seenMsgs, hash)
			}
		}
		cm.seenMsgsMu.Unlock()
	}
}

func (cm *ConnectionManager) healthCheck() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cm.mu.RLock()
		conns := make(map[string]*ManagedConnection, len(cm.connections))
		for addr, mc := range cm.connections {
			conns[addr] = mc
		}
		cm.mu.RUnlock()

		var dead []string
		var needPing []*ManagedConnection

		cm.pendingPingsMu.Lock()
		for addr, mc := range conns {
			if sent, pending := cm.pendingPings[addr]; pending {
				if time.Since(sent) > 10*time.Second {
					dead = append(dead, addr)
				}
				continue
			}

			cm.pendingPings[addr] = time.Now()
			needPing = append(needPing, mc)
		}
		cm.pendingPingsMu.Unlock()

		for _, addr := range dead {
			cm.pendingPingsMu.Lock()
			delete(cm.pendingPings, addr)
			cm.pendingPingsMu.Unlock()

			log.Printf("[ConnectionManager] Peer %s is unresponsive, removing", addr)
			fmt.Printf("\n%s\n> ", formatSystemMessage(addr+" disconnected (no response to ping)"))
			cm.removeConnection(addr)
		}

		for _, mc := range needPing {
			go func(addr string) {
				cm.mu.RLock()
				current, ok := cm.connections[addr]
				cm.mu.RUnlock()
				if !ok {
					cm.pendingPingsMu.Lock()
					delete(cm.pendingPings, addr)
					cm.pendingPingsMu.Unlock()
					return
				}
				current.mu.Lock()
				_, err := current.conn.Write([]byte("PING\n"))
				current.mu.Unlock()
				if err != nil {
					cm.pendingPingsMu.Lock()
					delete(cm.pendingPings, addr)
					cm.pendingPingsMu.Unlock()
					cm.removeConnection(addr)
				}
			}(mc.peerAddr)
		}
	}
}

func (cm *ConnectionManager) ClearPing(addr string) {
	cm.pendingPingsMu.Lock()
	delete(cm.pendingPings, addr)
	cm.pendingPingsMu.Unlock()
}

func (cm *ConnectionManager) GetLocalAlias() string {
	return cm.localAlias
}

func (cm *ConnectionManager) MarkRelay(addr string) {
	cm.relayMu.Lock()
	defer cm.relayMu.Unlock()
	cm.relayAddrs[addr] = true
}

func (cm *ConnectionManager) IsRelayPeer(addr string) bool {
	cm.relayMu.RLock()
	defer cm.relayMu.RUnlock()
	return cm.relayAddrs[addr]
}

func (cm *ConnectionManager) GetRelayAddrs() []string {
	cm.relayMu.RLock()
	defer cm.relayMu.RUnlock()
	addrs := make([]string, 0, len(cm.relayAddrs))
	for addr := range cm.relayAddrs {
		addrs = append(addrs, addr)
	}
	return addrs
}

func (cm *ConnectionManager) GetOrCreate(ctx context.Context, peerAddr string) (*ManagedConnection, error) {
	return cm.getOrCreate(ctx, peerAddr)
}

func (cm *ConnectionManager) getOrCreate(ctx context.Context, peerAddr string) (*ManagedConnection, error) {
	cm.mu.RLock()
	mc, exists := cm.connections[peerAddr]
	cm.mu.RUnlock()

	if exists {
		return mc, nil
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if mc, exists := cm.connections[peerAddr]; exists {
		return mc, nil
	}

	var pc PeerConnection

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
		ClientSessionCache: cm.sessionCache,
		KeyLogWriter:       globalKeyLog,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no certificate from peer %s", peerAddr)
			}
			fp := sha256.Sum256(rawCerts[0])
			fpHex := hex.EncodeToString(fp[:])

			cm.tofuMu.Lock()
			defer cm.tofuMu.Unlock()

			if known, ok := cm.tofuFingerprints[peerAddr]; !ok {
				cm.tofuFingerprints[peerAddr] = fpHex
			} else if known != fpHex {
				return fmt.Errorf("TOFU: certificate fingerprint mismatch for %s (possible MITM)", peerAddr)
			}
			return nil
		},
	}

	if cm.useTCP {
		d := net.Dialer{Timeout: 10 * time.Second}
		rawConn, err := d.DialContext(ctx, "tcp", peerAddr)
		if err != nil {
			return nil, fmt.Errorf("tcp dial failed: %w", err)
		}

		tlsConn := tls.Client(rawConn, tlsConf)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("tls handshake failed: %w", err)
		}

		pc = &NetConnWrapper{Conn: tlsConn}

	} else {
		conn, err := quic.DialAddr(ctx, peerAddr, tlsConf, &quic.Config{
			MaxIdleTimeout:                 QUICIdleTimeout,
			KeepAlivePeriod:                QUICKeepAlive,
			MaxIncomingStreams:             QUICMaxIncomingStreams,
			MaxIncomingUniStreams:          QUICMaxIncomingUniStreams,
			InitialStreamReceiveWindow:     QUICInitialStreamWindow,
			InitialConnectionReceiveWindow: QUICInitialConnWindow,
			MaxStreamReceiveWindow:         QUICMaxStreamWindow,
			MaxConnectionReceiveWindow:     QUICMaxConnWindow,
			Allow0RTT:                      true,
			DisablePathMTUDiscovery:        false,
		})
		if err != nil {
			return nil, fmt.Errorf("quic dial failed: %w", err)
		}

		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			conn.CloseWithError(1, "stream open failed")
			return nil, fmt.Errorf("stream open failed: %w", err)
		}

		pc = &QuicConnectionWrapper{
			Stream: stream,
			Conn:   conn,
		}
	}

	var handshake string
	if cm.roomPassword != "" {
		handshake = fmt.Sprintf("JOIN:%s|%s|%s\n", cm.roomName, cm.localAlias, cm.roomPassword)
	} else {
		handshake = fmt.Sprintf("JOIN:%s|%s\n", cm.roomName, cm.localAlias)
	}
	if _, err := pc.Write([]byte(handshake)); err != nil {
		pc.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	mc = &ManagedConnection{
		conn:      pc,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}

	cm.connections[peerAddr] = mc

	go cm.readLoop(mc)

	return mc, nil
}

func (cm *ConnectionManager) readLoop(mc *ManagedConnection) {
	defer cm.removeConnection(mc.peerAddr)

	reader := bufio.NewReader(mc.conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if line == "PONG" {
			mc.mu.Lock()
			mc.lastUsed = time.Now()
			mc.mu.Unlock()
			cm.ClearPing(mc.peerAddr)
			continue
		}

		if line == "PING" {
			mc.mu.Lock()
			mc.conn.Write([]byte("PONG\n"))
			mc.mu.Unlock()
			continue
		}

		if strings.HasPrefix(line, "AUTH:FAILED") {
			reason := strings.TrimPrefix(line, "AUTH:FAILED|")
			if reason == "" {
				reason = "Authentication failed"
			}
			fmt.Printf("\n%s\n> ", formatSystemMessage("Authentication failed: "+reason))
			cm.removeConnection(mc.peerAddr)
			return
		}

		var formatted string
		var sender, message string

		if strings.HasPrefix(line, "SYSTEM:") {
			message = strings.TrimPrefix(line, "SYSTEM:")
			sender = "System"

			if cm.IsDuplicate(sender, message) {
				continue
			}
			formatted = formatSystemMessage(message)
		} else if strings.HasPrefix(line, "FROM:") {
			parts := strings.SplitN(strings.TrimPrefix(line, "FROM:"), "|", 2)
			if len(parts) == 2 {
				sender = parts[0]
				message = parts[1]

				if cm.IsDuplicate(sender, message) {
					continue
				}
				formatted = formatMessage(sender, message)
			} else {
				formatted = formatMessage(mc.peerAddr, line)
			}
		} else if strings.HasPrefix(line, "MSG:") {
			message := strings.TrimPrefix(line, "MSG:")
			formatted = formatMessage(mc.peerAddr, message)
		} else {
			formatted = formatMessage(mc.peerAddr, line)
		}

		fmt.Printf("\n%s\n> ", formatted)
	}
}

func (cm *ConnectionManager) removeConnection(peerAddr string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	found := false
	if mc, exists := cm.connections[peerAddr]; exists {
		mc.conn.Close()
		delete(cm.connections, peerAddr)
		found = true
	}

	cm.pendingPingsMu.Lock()
	delete(cm.pendingPings, peerAddr)
	cm.pendingPingsMu.Unlock()

	return found
}

func (cm *ConnectionManager) Send(ctx context.Context, peerAddr, message string) error {
	mc, err := cm.getOrCreate(ctx, peerAddr)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.lastUsed = time.Now()

	alias := sanitiseField(cm.localAlias)
	msg := sanitiseField(message)

	formatted := fmt.Sprintf("FROM:%s|%s\n", alias, msg)
	_, err = mc.conn.Write([]byte(formatted))
	return err
}

func (cm *ConnectionManager) ListConnected() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	addrs := make([]string, 0, len(cm.connections))
	for addr := range cm.connections {
		addrs = append(addrs, addr)
	}
	return addrs
}

func (cm *ConnectionManager) RegisterIncoming(peerAddr string, pc PeerConnection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, exists := cm.connections[peerAddr]; exists {
		return
	}

	mc := &ManagedConnection{
		conn:      pc,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}

	cm.connections[peerAddr] = mc
}

func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for addr, mc := range cm.connections {
		mc.conn.Close()
		delete(cm.connections, addr)
	}
}
