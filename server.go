/*
server.go - QUIC and TCP Server and Room Management

This file implements the server that handles all incoming peer connections.
It supports both QUIC (UDP) and TCP transports for flexibility.
It manages chat rooms, processes incoming messages, and broadcasts to room members.

Key Responsibilities:
  - Accept incoming QUIC and TCP connections from peers
  - Manage room membership (join/leave)
  - Route and broadcast messages within rooms
  - Generate unique aliases for peers
  - Coordinate with ConnectionManager for bidirectional messaging

Message Protocol (incoming):
  - JOIN:<roomName>              - Request to join a room
  - FROM:<alias>|<message>       - Message from another peer
  - MSG:<message>                - Simple message (legacy format)
  - <plain text>                 - Raw message (if already in room)

Message Protocol (outgoing):
  - SYSTEM:<message>             - System notifications (join/leave)
  - FROM:<alias>|<message>       - Broadcast message to room peers
*/
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"crypto/tls"

	"github.com/quic-go/quic-go"
)

// Server handles incoming QUIC/TCP connections and manages chat rooms.
type Server struct {
	quicListener    *quic.Listener                   // QUIC listener
	tcpListener     net.Listener                     // TCP listener
	rooms           map[string]*Room                 // Active rooms by name
	roomsMu         sync.RWMutex                     // Thread-safe room access
	localPort       int                              // Port number we're listening on
	onMessage       func(from, room, message string) // Callback for displaying user messages
	onSystemMessage func(message string)             // Callback for system messages (join/leave)
	connManager     *ConnectionManager               // Reference to connection manager for bidirectional messaging
}

type Room struct {
	name    string
	peers   map[string]*Peer
	peersMu sync.RWMutex
}

type Peer struct {
	addr   string
	alias  string
	conn   PeerConnection // Abstracted connection (QUIC stream or TCP conn)
	room   *Room
}

// generateAlias creates a random alias for a peer
func generateAlias() string {
	adjectives := []string{"Swift", "Bold", "Clever", "Bright", "Quick", "Sharp", "Wise", "Calm", "Brave", "Cool"}
	nouns := []string{"Fox", "Eagle", "Wolf", "Hawk", "Lion", "Tiger", "Bear", "Deer", "Bird", "Fish"}

	adj := adjectives[rand.Intn(len(adjectives))]
	noun := nouns[rand.Intn(len(nouns))]
	num := rand.Intn(999) + 1

	return fmt.Sprintf("%s%s%d", adj, noun, num)
}

func NewServer(addr string, onMessage func(from, room, message string), onSystemMessage func(message string), connManager *ConnectionManager) (*Server, error) {
	tlsConfig := generateTLSConfig()

	// 1. Setup QUIC listener with AGGRESSIVE optimized config (from research)
	// See: "Optimizing quic-go for Localhost Latency.md"
	quicListener, err := quic.ListenAddr(addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:                 5 * time.Minute,  // Longer idle for persistent connections
		KeepAlivePeriod:                30 * time.Second, // Less frequent keep-alives
		MaxIncomingStreams:             1000,             // High concurrency
		MaxIncomingUniStreams:          1000,
		InitialStreamReceiveWindow:     6 * 1024 * 1024,  // 6 MB (research recommended)
		InitialConnectionReceiveWindow: 15 * 1024 * 1024, // 15 MB
		MaxStreamReceiveWindow:         16 * 1024 * 1024, // 16 MB
		MaxConnectionReceiveWindow:     64 * 1024 * 1024, // 64 MB (massive for localhost)
		Allow0RTT:                      true,             // Enable 0-RTT for faster reconnections
		DisablePathMTUDiscovery:        false,            // Enable for better packet sizes
	})
	if err != nil {
		return nil, fmt.Errorf("quic listen error: %w", err)
	}

	// Extract port from QUIC listener to ensure TCP binds to the same port
	_, portStr, _ := net.SplitHostPort(quicListener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	
	// If addr was "0.0.0.0:0", we now know the port. constructing "0.0.0.0:PORT"
	host, _, _ := net.SplitHostPort(addr)
	if host == "" {
		host = "0.0.0.0" 
	}
	tcpAddr := fmt.Sprintf("%s:%d", host, port)

	// 2. Setup TCP listener (TLS)
	tcpListener, err := tls.Listen("tcp", tcpAddr, tlsConfig)
	if err != nil {
		quicListener.Close()
		return nil, fmt.Errorf("tcp listen error: %w", err)
	}

	s := &Server{
		quicListener:    quicListener,
		tcpListener:     tcpListener,
		rooms:           make(map[string]*Room),
		localPort:       port,
		onMessage:       onMessage,
		onSystemMessage: onSystemMessage,
		connManager:     connManager,
	}

	// start accept loops for both transports
	go s.acceptLoopQUIC()
	go s.acceptLoopTCP()

	log.Printf("[Server] Listening on port %s (QUIC+TCP)", colorPort(port))
	return s, nil
}

func (s *Server) Port() int {
	return s.localPort
}

// Accept QUIC connections
func (s *Server) acceptLoopQUIC() {
	for {
		conn, err := s.quicListener.Accept(context.Background())
		if err != nil {
			// If error, the listener is likely closed or broken.
			// Just return to exit the loop.
			// log.Printf("[Server] QUIC Accept error: %v", err)
			return
		}

		go s.handleQUICConnection(conn)
	}
}

// Handle individual QUIC connection
func (s *Server) handleQUICConnection(conn *quic.Conn) {
	peerAddr := conn.RemoteAddr().String()
	
	// Accept the stream (this is where we wait for the peer to initiate 'chat')
	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		// Only log real errors, not just disconnects
		return 
	}

	// Wrap as PeerConnection
	pc := &QuicConnectionWrapper{
		Stream: stream,
		Conn:   conn,
		// Session alias for convenience if needed, essentially Conn
	}

	s.handlePeerConnection(pc, peerAddr)
}

// Accept TCP connections
func (s *Server) acceptLoopTCP() {
	for {
		conn, err := s.tcpListener.Accept()
		if err != nil {
			// If listener closed, return
			return
		}
		
		go s.handleTCPConnection(conn)
	}
}

// Handle individual TCP connection
func (s *Server) handleTCPConnection(conn net.Conn) {
	peerAddr := conn.RemoteAddr().String()
	
	// Wrap as PeerConnection
	pc := &NetConnWrapper{
		Conn: conn,
	}

	s.handlePeerConnection(pc, peerAddr)
}

// Generic handler for any PeerConnection (QUIC or TCP)
func (s *Server) handlePeerConnection(pc PeerConnection, peerAddr string) {
	log.Printf("[Server] New connection from %s", peerAddr)

	// Register incoming connection in ConnectionManager for bidirectional messaging
	if s.connManager != nil {
		s.connManager.RegisterIncoming(peerAddr, pc)
	}

	peer := &Peer{
		addr:   peerAddr,
		alias:  generateAlias(),
		conn:   pc,
	}

	// Read messages
	reader := bufio.NewReader(pc)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			s.removePeer(peer)
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		s.handleMessage(peer, line)
	}
}

func (s *Server) handleMessage(peer *Peer, message string) {
	// Parse message type
	if strings.HasPrefix(message, "JOIN:") {
		roomName := strings.TrimPrefix(message, "JOIN:")
		s.joinRoom(peer, roomName)
		return
	}

	// Handle FROM:alias|content format (sent by ConnectionManager)
	if strings.HasPrefix(message, "FROM:") {
		parts := strings.SplitN(strings.TrimPrefix(message, "FROM:"), "|", 2)
		if len(parts) == 2 {
			senderAlias := parts[0]
			content := parts[1]
			// Update peer's alias to match what they claim (trust the sender's alias)
			peer.alias = senderAlias
			if peer.room != nil {
				// Broadcast to other peers (relay for mesh network)
				s.broadcastToRoom(peer.room, peer.addr, content)
				// Display locally (only if not already seen - prevents duplicates in mesh)
				if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(senderAlias, content) {
					s.onMessage(senderAlias, peer.room.name, content)
				}
			}
		}
		return
	}

	if strings.HasPrefix(message, "MSG:") {
		content := strings.TrimPrefix(message, "MSG:")
		if peer.room != nil {
			s.broadcastToRoom(peer.room, peer.addr, content)
			// Display locally
			if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, content) {
				s.onMessage(peer.alias, peer.room.name, content)
			}
		}
		return
	}

	// Default: treat as message if already in room
	if peer.room != nil {
		s.broadcastToRoom(peer.room, peer.addr, message)
		// Display locally
		if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, message) {
			s.onMessage(peer.alias, peer.room.name, message)
		}
	}
}

func (s *Server) joinRoom(peer *Peer, roomName string) {
	s.roomsMu.Lock()
	room, exists := s.rooms[roomName]
	if !exists {
		room = &Room{
			name:  roomName,
			peers: make(map[string]*Peer),
		}
		s.rooms[roomName] = room
	}
	s.roomsMu.Unlock()

	room.peersMu.Lock()
	room.peers[peer.addr] = peer
	peer.room = room
	room.peersMu.Unlock()

	// Broadcast join message to room AND display locally
	joinMsg := fmt.Sprintf("%s (%s) joined the chat", peer.alias, peer.addr)
	s.broadcastToRoom(room, "", joinMsg)

	// Display join message locally
	if s.onSystemMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate("System", joinMsg) {
		s.onSystemMessage(joinMsg)
	}

	log.Printf("[Server] Peer %s (%s) joined room '%s'", peer.alias, peer.addr, roomName)
}

func (s *Server) broadcastToRoom(room *Room, senderAddr, message string) {
	room.peersMu.RLock()
	peers := make([]*Peer, 0, len(room.peers))
	var senderAlias string
	for _, p := range room.peers {
		if p.addr == senderAddr {
			senderAlias = p.alias
		} else {
			peers = append(peers, p)
		}
	}
	room.peersMu.RUnlock()

	// Use alias if available, otherwise use address
	from := senderAlias
	isSystemMsg := false
	if from == "" && senderAddr != "" {
		from = senderAddr
	} else if from == "" {
		from = "System"
		isSystemMsg = true
	}

	var formatted string
	if isSystemMsg {
		formatted = fmt.Sprintf("SYSTEM:%s\n", message)
	} else {
		formatted = fmt.Sprintf("FROM:%s|%s\n", from, message)
	}

	// Pre-convert to bytes once (avoid repeated allocation)
	msgBytes := []byte(formatted)

	// Parallel broadcast with goroutines for better performance
	// With 10 peers, this reduces broadcast time from 10ms to ~1ms
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p *Peer) {
			defer wg.Done()
			if _, err := p.conn.Write(msgBytes); err != nil {
				log.Printf("[Server] Broadcast error to %s: %v", p.addr, err)
			}
		}(peer)
	}
	wg.Wait()
}

func (s *Server) removePeer(peer *Peer) {
	if peer.room != nil {
		peer.room.peersMu.Lock()
		delete(peer.room.peers, peer.addr)
		peer.room.peersMu.Unlock()

		// Broadcast leave message AND display locally
		leaveMsg := fmt.Sprintf("%s (%s) left the chat", peer.alias, peer.addr)
		s.broadcastToRoom(peer.room, "", leaveMsg)

		// Display leave message locally
		if s.onSystemMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate("System", leaveMsg) {
			s.onSystemMessage(leaveMsg)
		}

		log.Printf("[Server] Peer %s left room '%s'", peer.addr, peer.room.name)
	}
	peer.conn.Close()
}

func (s *Server) Close() error {
	var err error
	if s.quicListener != nil {
		err = s.quicListener.Close()
	}
	if s.tcpListener != nil {
		errTcp := s.tcpListener.Close()
		if err == nil {
			err = errTcp
		}
	}
	return err
}
