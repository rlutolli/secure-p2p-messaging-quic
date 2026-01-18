/*
connection_manager.go - Outgoing Connection Pool and Message Deduplication

This file manages persistent connections to peers, handling outgoing messages
and providing efficient connection reuse. It now supports both QUIC and TCP
transports.
*/
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// generateAlias creates a random alias for the local user
func generateLocalAlias() string {
	adjectives := []string{"Swift", "Bold", "Clever", "Bright", "Quick", "Sharp", "Wise", "Calm", "Brave", "Cool"}
	nouns := []string{"Fox", "Eagle", "Wolf", "Hawk", "Lion", "Tiger", "Bear", "Deer", "Bird", "Fish"}

	adj := adjectives[rand.Intn(len(adjectives))]
	noun := nouns[rand.Intn(len(nouns))]
	num := rand.Intn(999) + 1

	return fmt.Sprintf("%s%s%d", adj, noun, num)
}

// ConnectionManager handles persistent connections to peers
type ConnectionManager struct {
	mu          sync.RWMutex
	connections map[string]*ManagedConnection // peerAddr -> connection
	localPort   int
	roomName    string
	localAlias  string // Our alias for this session
	useTCP      bool   // Preference for TCP over QUIC

	// Message deduplication
	seenMsgsMu sync.Mutex
	seenMsgs   map[string]time.Time // hash -> timestamp (for cleanup)
}

type ManagedConnection struct {
	conn      PeerConnection // Abstracted connection
	peerAddr  string
	createdAt time.Time
	lastUsed  time.Time  // Track last usage for health checks
	healthy   bool       // Connection health flag
	mu        sync.Mutex
}

func NewConnectionManager(localPort int, roomName string, useTCP bool) *ConnectionManager {
	cm := &ConnectionManager{
		connections: make(map[string]*ManagedConnection),
		localPort:   localPort,
		roomName:    roomName,
		localAlias:  generateLocalAlias(),
		seenMsgs:    make(map[string]time.Time),
		useTCP:      useTCP,
	}

	// Start cleanup goroutine for old message hashes
	go cm.cleanupSeenMsgs()

	// Start connection health check goroutine
	go cm.healthCheck()

	return cm
}

// IsDuplicate checks if we've seen this message recently (exported for Server use)
func (cm *ConnectionManager) IsDuplicate(sender, message string) bool {
	// Create hash of sender + message
	hash := sha256.Sum256([]byte(sender + "|" + message))
	hashStr := hex.EncodeToString(hash[:8]) // Use first 8 bytes

	cm.seenMsgsMu.Lock()
	defer cm.seenMsgsMu.Unlock()

	if _, seen := cm.seenMsgs[hashStr]; seen {
		return true // Duplicate
	}

	// Mark as seen
	cm.seenMsgs[hashStr] = time.Now()
	return false
}

// cleanupSeenMsgs periodically removes old message hashes
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

// healthCheck periodically checks connection health and cleans up stale connections
func (cm *ConnectionManager) healthCheck() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cm.mu.RLock()
		staleConnections := make([]string, 0)
		for addr, mc := range cm.connections {
			mc.mu.Lock()
			// Mark connections as unhealthy if idle for too long
			if time.Since(mc.lastUsed) > 120*time.Second {
				mc.healthy = false
				staleConnections = append(staleConnections, addr)
			}
			mc.mu.Unlock()
		}
		cm.mu.RUnlock()

		// Log stale connections (don't remove them, just mark unhealthy)
		for _, addr := range staleConnections {
			log.Printf("[ConnectionManager] Connection to %s marked unhealthy (idle > 120s)", addr)
		}
	}
}

// GetLocalAlias returns our alias for display
func (cm *ConnectionManager) GetLocalAlias() string {
	return cm.localAlias
}

// GetOrCreate returns existing connection or creates new one (exported)
func (cm *ConnectionManager) GetOrCreate(ctx context.Context, peerAddr string) (*ManagedConnection, error) {
	return cm.getOrCreate(ctx, peerAddr)
}

// getOrCreate is the internal implementation
func (cm *ConnectionManager) getOrCreate(ctx context.Context, peerAddr string) (*ManagedConnection, error) {
	// Check for existing connection
	cm.mu.RLock()
	if mc, exists := cm.connections[peerAddr]; exists {
		cm.mu.RUnlock()
		return mc, nil
	}
	cm.mu.RUnlock()

	// Create new connection
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Double-check after acquiring write lock
	if mc, exists := cm.connections[peerAddr]; exists {
		return mc, nil
	}

	var pc PeerConnection

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
	}

	if cm.useTCP {
		// TCP Dial
		d := net.Dialer{Timeout: 10 * time.Second}
		rawConn, err := d.DialContext(ctx, "tcp", peerAddr)
		if err != nil {
			return nil, fmt.Errorf("tcp dial failed: %w", err)
		}
		
		tlsConn := tls.Client(rawConn, tlsConf)
		// TLS Handshake is implicit on first Read/Write, but good to check
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("tls handshake failed: %w", err)
		}
		
		pc = &NetConnWrapper{Conn: tlsConn}

	} else {
		// QUIC Dial with AGGRESSIVE optimized config (from research)
		// See: "Optimizing quic-go for Localhost Latency.md"
		conn, err := quic.DialAddr(ctx, peerAddr, tlsConf, &quic.Config{
			MaxIdleTimeout:                 5 * time.Minute,
			KeepAlivePeriod:                30 * time.Second,
			MaxIncomingStreams:             1000,
			MaxIncomingUniStreams:          1000,
			InitialStreamReceiveWindow:     6 * 1024 * 1024,  // 6 MB
			InitialConnectionReceiveWindow: 15 * 1024 * 1024, // 15 MB
			MaxStreamReceiveWindow:         16 * 1024 * 1024, // 16 MB
			MaxConnectionReceiveWindow:     64 * 1024 * 1024, // 64 MB
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

	// Send initial handshake with room info
	handshake := fmt.Sprintf("JOIN:%s\n", cm.roomName)
	if _, err := pc.Write([]byte(handshake)); err != nil {
		pc.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	mc := &ManagedConnection{
		conn:      pc,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
		healthy:   true,
	}

	cm.connections[peerAddr] = mc

	// Start reader goroutine for incoming messages
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

		// Parse message format: "SYSTEM:message", "FROM:alias|message", or "MSG:message"
		var formatted string
		var sender, message string

		if strings.HasPrefix(line, "SYSTEM:") {
			// System message (join/leave): use [System] format
			message = strings.TrimPrefix(line, "SYSTEM:")
			sender = "System"

			// Check for duplicate system messages
			if cm.IsDuplicate(sender, message) {
				continue
			}
			formatted = formatSystemMessage(message)
		} else if strings.HasPrefix(line, "FROM:") {
			// Broadcast message from Server: "FROM:alias|message"
			parts := strings.SplitN(strings.TrimPrefix(line, "FROM:"), "|", 2)
			if len(parts) == 2 {
				sender = parts[0]
				message = parts[1]

				// Check for duplicate messages
				if cm.IsDuplicate(sender, message) {
					continue
				}
				formatted = formatMessage(sender, message)
			} else {
				formatted = formatMessage(mc.peerAddr, line)
			}
		} else if strings.HasPrefix(line, "MSG:") {
			// Direct message: "MSG:message" - strip prefix, use peer address as sender
			message := strings.TrimPrefix(line, "MSG:")
			formatted = formatMessage(mc.peerAddr, message)
		} else {
			// Fallback: treat as raw message
			formatted = formatMessage(mc.peerAddr, line)
		}

		fmt.Printf("\n%s\n> ", formatted)
	}
}

func (cm *ConnectionManager) removeConnection(peerAddr string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if mc, exists := cm.connections[peerAddr]; exists {
		mc.conn.Close()
		delete(cm.connections, peerAddr)
	}
}

// Send message to a specific peer
func (cm *ConnectionManager) Send(ctx context.Context, peerAddr, message string) error {
	mc, err := cm.getOrCreate(ctx, peerAddr)
	if err != nil {
		return err
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Update lastUsed for connection health tracking
	mc.lastUsed = time.Now()

	// Format message with FROM: prefix including our alias so receiver knows who sent it
	formatted := fmt.Sprintf("FROM:%s|%s\n", cm.localAlias, message)
	_, err = mc.conn.Write([]byte(formatted))
	return err
}

// Broadcast sends to all connected peers
func (cm *ConnectionManager) Broadcast(ctx context.Context, message string) []error {
	cm.mu.RLock()
	addrs := make([]string, 0, len(cm.connections))
	for addr := range cm.connections {
		addrs = append(addrs, addr)
	}
	cm.mu.RUnlock()

	var errors []error
	for _, addr := range addrs {
		if err := cm.Send(ctx, addr, message); err != nil {
			errors = append(errors, fmt.Errorf("%s: %w", addr, err))
		}
	}
	return errors
}

// RegisterIncoming registers an incoming connection (from Server) in the ConnectionManager
// This enables bidirectional messaging
func (cm *ConnectionManager) RegisterIncoming(peerAddr string, pc PeerConnection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Check if already registered
	if _, exists := cm.connections[peerAddr]; exists {
		return // Already registered
	}

	mc := &ManagedConnection{
		conn:      pc,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
	}

	cm.connections[peerAddr] = mc
	// Do NOT start readLoop here - Server.handleConnection already reads from this connection
}

// Close all connections
func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for addr, mc := range cm.connections {
		mc.conn.Close()
		delete(cm.connections, addr)
	}
}
