/*
server.go - QUIC and TCP Server and Room Management

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
	"net"
	"strings"
	"sync"
	"time"

	"crypto/subtle"
	"crypto/tls"

	"github.com/quic-go/quic-go"
)

type Server struct {
	quicListener    *quic.Listener
	tcpListener     net.Listener
	rooms           map[string]*Room
	roomsMu         sync.RWMutex
	localPort       int
	onMessage       func(from, room, message string)
	onSystemMessage func(message string)
	connManager     *ConnectionManager
	setupSem        chan struct{}
}

type Room struct {
	name         string
	passwordHash string
	peers        map[string]*Peer
	peersMu      sync.RWMutex
}

type Peer struct {
	addr  string
	alias string
	conn  PeerConnection
	room  *Room
}

func NewServer(addr string, onMessage func(from, room, message string), onSystemMessage func(message string), connManager *ConnectionManager) (*Server, error) {
	tlsConfig := generateTLSConfig()

	quicListener, err := quic.ListenAddr(addr, tlsConfig, &quic.Config{
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
		return nil, fmt.Errorf("quic listen error: %w", err)
	}

	_, portStr, _ := net.SplitHostPort(quicListener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	host, _, _ := net.SplitHostPort(addr)
	if host == "" {
		host = "0.0.0.0"
	}
	tcpAddr := fmt.Sprintf("%s:%d", host, port)

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
		setupSem:        make(chan struct{}, MaxConcurrentHandshakes),
	}

	go s.acceptLoopQUIC()
	go s.acceptLoopTCP()

	log.Printf("[Server] Listening on port %s (QUIC+TCP)", colorPort(port))
	return s, nil
}

func (s *Server) Port() int {
	return s.localPort
}

// acquireSetup and releaseSetup bound concurrent connection setup. They are
// nil-safe so a Server constructed without a semaphore (e.g. in tests) simply
// runs setup unbounded instead of deadlocking on a nil channel.
func (s *Server) acquireSetup() {
	if s.setupSem != nil {
		s.setupSem <- struct{}{}
	}
}

func (s *Server) releaseSetup() {
	if s.setupSem != nil {
		<-s.setupSem
	}
}

func (s *Server) acceptLoopQUIC() {
	for {
		conn, err := s.quicListener.Accept(context.Background())
		if err != nil {
			return
		}
		go s.handleQUICConnection(conn)
	}
}

func (s *Server) handleQUICConnection(conn *quic.Conn) {
	peerAddr := conn.RemoteAddr().String()

	// Bound concurrent setup so a burst of peers cannot overwhelm the relay's
	// accept path. The slot is held only for the setup phase (waiting for the
	// peer's first stream), not for the lifetime of the connection.
	s.acquireSetup()

	ctx, cancel := context.WithTimeout(context.Background(), HandshakeSetupTimeout)
	stream, err := conn.AcceptStream(ctx)
	cancel()
	s.releaseSetup()
	if err != nil {
		conn.CloseWithError(0, "stream accept timeout")
		return
	}

	pc := &QuicConnectionWrapper{
		Stream: stream,
		Conn:   conn,
	}

	s.handlePeerConnection(pc, peerAddr)
}

func (s *Server) acceptLoopTCP() {
	for {
		conn, err := s.tcpListener.Accept()
		if err != nil {
			return
		}
		go s.handleTCPConnection(conn)
	}
}

func (s *Server) handleTCPConnection(conn net.Conn) {
	peerAddr := conn.RemoteAddr().String()

	// Bound concurrent setup and force the TLS handshake to complete (with a
	// deadline) under that bound, so a burst of peers cannot pile up unbounded
	// crypto work. The deadline is cleared before entering the read loop.
	s.acquireSetup()
	if tlsConn, ok := conn.(*tls.Conn); ok {
		_ = tlsConn.SetDeadline(time.Now().Add(HandshakeSetupTimeout))
		if err := tlsConn.Handshake(); err != nil {
			_ = tlsConn.SetDeadline(time.Time{})
			tlsConn.Close()
			s.releaseSetup()
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
	}
	s.releaseSetup()

	pc := &NetConnWrapper{Conn: conn}
	s.handlePeerConnection(pc, peerAddr)
}

func (s *Server) handlePeerConnection(pc PeerConnection, peerAddr string) {
	log.Printf("[Server] New connection from %s", peerAddr)

	if s.connManager != nil {
		s.connManager.RegisterIncoming(peerAddr, pc)
	}

	peer := &Peer{
		addr:  peerAddr,
		alias: generateAlias(),
		conn:  pc,
	}

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
	if message == "PING" {
		if s.connManager != nil {
			s.connManager.mu.RLock()
			mc, ok := s.connManager.connections[peer.addr]
			s.connManager.mu.RUnlock()
			if ok {
				mc.mu.Lock()
				peer.conn.Write([]byte("PONG\n"))
				mc.mu.Unlock()
				return
			}
		}
		peer.conn.Write([]byte("PONG\n"))
		return
	}
	if message == "PONG" {
		if s.connManager != nil {
			s.connManager.ClearPing(peer.addr)
		}
		return
	}

	if strings.HasPrefix(message, "JOIN:") {
		payload := strings.TrimPrefix(message, "JOIN:")
		parts := strings.SplitN(payload, "|", 3)
		roomName := parts[0]
		if len(parts) >= 2 && parts[1] != "" {
			peer.alias = parts[1]
		}
		var providedPassword string
		if len(parts) == 3 {
			providedPassword = parts[2]
		}

		s.roomsMu.RLock()
		room, exists := s.rooms[roomName]
		s.roomsMu.RUnlock()

		passwordHash := ""
		if providedPassword != "" {
			derived := DeriveRoomKey(roomName, providedPassword)
			if exists && room.passwordHash != "" {
				if subtle.ConstantTimeCompare([]byte(room.passwordHash), derived.roomKey) != 1 {
					peer.conn.Write([]byte("AUTH:FAILED|Incorrect password\n"))
					s.removePeer(peer)
					return
				}
			} else {
				passwordHash = string(derived.roomKey)
			}
		}

		s.joinRoom(peer, roomName, passwordHash)
		return
	}

	if strings.HasPrefix(message, "FROM:") {
		parts := strings.SplitN(strings.TrimPrefix(message, "FROM:"), "|", 2)
		if len(parts) == 2 {
			content := parts[1]
			if peer.room != nil {
				s.broadcastToRoom(peer.room, peer.addr, content)
				if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, content) {
					s.onMessage(peer.alias, peer.room.name, content)
				}
			}
		}
		return
	}

	if strings.HasPrefix(message, "MSG:") {
		content := strings.TrimPrefix(message, "MSG:")
		if peer.room != nil {
			s.broadcastToRoom(peer.room, peer.addr, content)
			if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, content) {
				s.onMessage(peer.alias, peer.room.name, content)
			}
		}
		return
	}

	if peer.room != nil {
		s.broadcastToRoom(peer.room, peer.addr, message)
		if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, message) {
			s.onMessage(peer.alias, peer.room.name, message)
		}
	}
}

func (s *Server) joinRoom(peer *Peer, roomName string, passwordHash string) {
	s.roomsMu.Lock()
	room, exists := s.rooms[roomName]
	if !exists {
		room = &Room{
			name:         roomName,
			passwordHash: passwordHash,
			peers:        make(map[string]*Peer),
		}
		s.rooms[roomName] = room
	}
	s.roomsMu.Unlock()

	room.peersMu.Lock()
	room.peers[peer.addr] = peer
	peer.room = room
	room.peersMu.Unlock()

	joinMsg := fmt.Sprintf("%s (%s) joined the chat", peer.alias, peer.addr)
	s.broadcastToRoom(room, peer.addr, joinMsg)

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

	msgBytes := []byte(formatted)

	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p *Peer) {
			defer wg.Done()
			var err error
			if s.connManager != nil {
				s.connManager.mu.RLock()
				mc, ok := s.connManager.connections[p.addr]
				s.connManager.mu.RUnlock()
				if ok {
					mc.mu.Lock()
					_, err = p.conn.Write(msgBytes)
					mc.mu.Unlock()
				} else {
					_, err = p.conn.Write(msgBytes)
				}
			} else {
				_, err = p.conn.Write(msgBytes)
			}
			if err != nil {
				log.Printf("[Server] Broadcast error to %s: %v", p.addr, err)
			}
		}(peer)
	}
	wg.Wait()
}

func (s *Server) removePeer(peer *Peer) {
	if peer.room != nil {
		var roomName string
		peer.room.peersMu.Lock()
		delete(peer.room.peers, peer.addr)
		empty := len(peer.room.peers) == 0
		if empty {
			roomName = peer.room.name
		}
		peer.room.peersMu.Unlock()

		if empty && roomName != "" {
			s.roomsMu.Lock()
			if room, exists := s.rooms[roomName]; exists {
				room.peersMu.RLock()
				stillEmpty := len(room.peers) == 0
				room.peersMu.RUnlock()
				if stillEmpty {
					delete(s.rooms, roomName)
				}
			}
			s.roomsMu.Unlock()
		}

		leaveMsg := fmt.Sprintf("%s (%s) left the chat", peer.alias, peer.addr)
		s.broadcastToRoom(peer.room, "", leaveMsg)

		if s.onSystemMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate("System", leaveMsg) {
			s.onSystemMessage(leaveMsg)
		}

		log.Printf("[Server] Peer %s left room '%s'", peer.addr, peer.room.name)
	}
	if s.connManager != nil {
		if !s.connManager.removeConnection(peer.addr) {
			peer.conn.Close()
		}
	} else {
		peer.conn.Close()
	}
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
