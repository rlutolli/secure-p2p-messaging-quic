package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Server handles incoming P2P connections
type Server struct {
	listener  *quic.Listener
	rooms     map[string]*Room
	roomsMu   sync.RWMutex
	localPort int
	onMessage func(from, room, message string)
}

type Room struct {
	name    string
	peers   map[string]*Peer
	peersMu sync.RWMutex
}

type Peer struct {
	addr   string
	conn   *quic.Conn
	stream *quic.Stream
	room   *Room
}

func NewServer(addr string, onMessage func(from, room, message string)) (*Server, error) {
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
		listener:  listener,
		rooms:     make(map[string]*Room),
		localPort: port,
		onMessage: onMessage,
	}

	go s.acceptLoop()

	log.Printf("[Server] Listening on port %d", port)
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

	peer := &Peer{
		addr:   peerAddr,
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

	if strings.HasPrefix(message, "MSG:") {
		content := strings.TrimPrefix(message, "MSG:")
		if peer.room != nil {
			s.broadcastToRoom(peer.room, peer.addr, content)
			if s.onMessage != nil {
				s.onMessage(peer.addr, peer.room.name, content)
			}
		}
		return
	}

	// Default: treat as message if already in room
	if peer.room != nil {
		s.broadcastToRoom(peer.room, peer.addr, message)
		if s.onMessage != nil {
			s.onMessage(peer.addr, peer.room.name, message)
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

	log.Printf("[Server] Peer %s joined room '%s'", peer.addr, roomName)
}

func (s *Server) broadcastToRoom(room *Room, senderAddr, message string) {
	room.peersMu.RLock()
	peers := make([]*Peer, 0, len(room.peers))
	for _, p := range room.peers {
		if p.addr != senderAddr {
			peers = append(peers, p)
		}
	}
	room.peersMu.RUnlock()

	formatted := fmt.Sprintf("FROM:%s|%s\n", senderAddr, message)

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

		log.Printf("[Server] Peer %s left room '%s'", peer.addr, peer.room.name)
	}
	peer.conn.CloseWithError(0, "disconnected")
}

func (s *Server) Close() error {
	return s.listener.Close()
}
