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

	"github.com/quic-go/quic-go"
)

// Server handles incoming P2P connections
type Server struct {
	listener        *quic.Listener
	rooms           map[string]*Room
	roomsMu         sync.RWMutex
	localPort       int
	onMessage       func(from, room, message string)
	onSystemMessage func(message string) // Callback for system messages (join/leave)
	connManager     *ConnectionManager   // Reference to connection manager for bidirectional messaging
}

type Room struct {
	name    string
	peers   map[string]*Peer
	peersMu sync.RWMutex
}

type Peer struct {
	addr   string
	alias  string
	conn   *quic.Conn
	stream *quic.Stream
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

	listener, err := quic.ListenAddr(addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("listen error: %w", err)
	}

	// Extract port
	_, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	s := &Server{
		listener:        listener,
		rooms:           make(map[string]*Room),
		localPort:       port,
		onMessage:       onMessage,
		onSystemMessage: onSystemMessage,
		connManager:     connManager,
	}

	go s.acceptLoop()

	log.Printf("[Server] Listening on port %s", colorPort(port))
	return s, nil
}

func (s *Server) Port() int {
	return s.localPort
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept(context.Background())
		if err != nil {
			log.Printf("[Server] Accept error: %v", err)
			continue
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn *quic.Conn) {
	peerAddr := conn.RemoteAddr().String()
	log.Printf("[Server] New connection from %s", peerAddr)

	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		log.Printf("[Server] Accept stream error: %v", err)
		return
	}

	// Register incoming connection in ConnectionManager for bidirectional messaging
	if s.connManager != nil {
		s.connManager.RegisterIncoming(peerAddr, conn, stream)
	}

	peer := &Peer{
		addr:   peerAddr,
		alias:  generateAlias(),
		conn:   conn,
		stream: stream,
	}

	// Read messages using buffered reader for proper line handling
	reader := bufio.NewReader(stream)

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
	// Broadcast to other peers in room so they receive it (mesh relay)
	// Receivers will deduplicate if they get the same message from multiple sources
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
			// Display locally (only if not already seen - prevents duplicates in mesh)
			if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, content) {
				s.onMessage(peer.alias, peer.room.name, content)
			}
		}
		return
	}

	// Default: treat as message if already in room
	if peer.room != nil {
		s.broadcastToRoom(peer.room, peer.addr, message)
		// Display locally (only if not already seen - prevents duplicates in mesh)
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

	// Display join message locally (so room creator sees it, check for duplicates)
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
	// System messages use "System" as sender
	from := senderAlias
	isSystemMsg := false
	if from == "" && senderAddr != "" {
		from = senderAddr
	} else if from == "" {
		from = "System"
		isSystemMsg = true
	}

	// System messages use SYSTEM: prefix for special formatting on receiver
	var formatted string
	if isSystemMsg {
		formatted = fmt.Sprintf("SYSTEM:%s\n", message)
	} else {
		formatted = fmt.Sprintf("FROM:%s|%s\n", from, message)
	}

	for _, peer := range peers {
		if _, err := peer.stream.Write([]byte(formatted)); err != nil {
			log.Printf("[Server] Broadcast error to %s: %v", peer.addr, err)
		}
	}
}

func (s *Server) removePeer(peer *Peer) {
	if peer.room != nil {
		peer.room.peersMu.Lock()
		delete(peer.room.peers, peer.addr)
		peer.room.peersMu.Unlock()

		// Broadcast leave message AND display locally
		leaveMsg := fmt.Sprintf("%s (%s) left the chat", peer.alias, peer.addr)
		s.broadcastToRoom(peer.room, "", leaveMsg)

		// Display leave message locally (check for duplicates)
		if s.onSystemMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate("System", leaveMsg) {
			s.onSystemMessage(leaveMsg)
		}

		log.Printf("[Server] Peer %s left room '%s'", peer.addr, peer.room.name)
	}
	peer.conn.CloseWithError(0, "disconnected")
}

func (s *Server) Close() error {
	return s.listener.Close()
}
