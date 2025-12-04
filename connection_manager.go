package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
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
}

type ManagedConnection struct {
	conn      *quic.Conn
	stream    *quic.Stream
	peerAddr  string
	createdAt time.Time
	mu        sync.Mutex
}

func NewConnectionManager(localPort int, roomName string) *ConnectionManager {
	return &ConnectionManager{
		connections: make(map[string]*ManagedConnection),
		localPort:   localPort,
		roomName:    roomName,
		localAlias:  generateLocalAlias(),
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

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"p2p-messenger/1.0"},
	}

	conn, err := quic.DialAddr(ctx, peerAddr, tlsConf, &quic.Config{
		KeepAlivePeriod: 10 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		conn.CloseWithError(1, "stream open failed")
		return nil, fmt.Errorf("stream open failed: %w", err)
	}

	// Send initial handshake with room info
	handshake := fmt.Sprintf("JOIN:%s\n", cm.roomName)
	if _, err := stream.Write([]byte(handshake)); err != nil {
		conn.CloseWithError(1, "handshake failed")
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	mc := &ManagedConnection{
		conn:      conn,
		stream:    stream,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
	}

	cm.connections[peerAddr] = mc

	// Start reader goroutine for incoming messages
	go cm.readLoop(mc)

	return mc, nil
}

func (cm *ConnectionManager) readLoop(mc *ManagedConnection) {
	defer cm.removeConnection(mc.peerAddr)

	reader := bufio.NewReader(mc.stream)
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

		if strings.HasPrefix(line, "SYSTEM:") {
			// System message (join/leave): use [System] format
			message := strings.TrimPrefix(line, "SYSTEM:")
			formatted = formatSystemMessage(message)
		} else if strings.HasPrefix(line, "FROM:") {
			// Broadcast message from Server: "FROM:alias|message"
			parts := strings.SplitN(strings.TrimPrefix(line, "FROM:"), "|", 2)
			if len(parts) == 2 {
				sender := parts[0]
				message := parts[1]
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
		mc.conn.CloseWithError(0, "closing")
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

	// Format message with FROM: prefix including our alias so receiver knows who sent it
	formatted := fmt.Sprintf("FROM:%s|%s\n", cm.localAlias, message)
	_, err = mc.stream.Write([]byte(formatted))
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
// This enables bidirectional messaging - when someone connects to us, we can send messages back
// NOTE: Does NOT start readLoop - the Server already handles reading from this stream
func (cm *ConnectionManager) RegisterIncoming(peerAddr string, conn *quic.Conn, stream *quic.Stream) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Check if already registered
	if _, exists := cm.connections[peerAddr]; exists {
		return // Already registered
	}

	mc := &ManagedConnection{
		conn:      conn,
		stream:    stream,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
	}

	cm.connections[peerAddr] = mc
	// Do NOT start readLoop here - Server.handleConnection already reads from this stream
}

// Close all connections
func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for addr, mc := range cm.connections {
		mc.conn.CloseWithError(0, "shutdown")
		delete(cm.connections, addr)
	}
}
