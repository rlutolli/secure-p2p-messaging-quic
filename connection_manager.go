package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ConnectionManager handles persistent connections to peers
type ConnectionManager struct {
	mu          sync.RWMutex
	connections map[string]*ManagedConnection // peerAddr -> connection
	localPort   int
	roomName    string
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
	}
}

// GetOrCreate returns existing connection or creates new one
func (cm *ConnectionManager) GetOrCreate(ctx context.Context, peerAddr string) (*ManagedConnection, error) {
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

		// Parse message format: "FROM:senderAddr|message" or just "message"
		var displayMsg string
		if strings.HasPrefix(line, "FROM:") {
			// Extract sender and message
			parts := strings.SplitN(strings.TrimPrefix(line, "FROM:"), "|", 2)
			if len(parts) == 2 {
				sender := parts[0]
				displayMsg = parts[1]
				fmt.Printf("\n[%s]: %s\n> ", sender, displayMsg)
			} else {
				fmt.Printf("\n[%s]: %s\n> ", mc.peerAddr, line)
			}
		} else {
			fmt.Printf("\n[%s]: %s\n> ", mc.peerAddr, line)
		}
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
	mc, err := cm.GetOrCreate(ctx, peerAddr)
	if err != nil {
		return err
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Format message with MSG: prefix that server expects
	formatted := fmt.Sprintf("MSG:%s\n", message)
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

// Close all connections
func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for addr, mc := range cm.connections {
		mc.conn.CloseWithError(0, "shutdown")
		delete(cm.connections, addr)
	}
}
