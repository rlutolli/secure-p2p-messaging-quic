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
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

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
	// banList records addresses that are permanently blocked from joining any room.
	// An address is banned via /ban and unbanned via /unban. Kick (without ban)
	// only removes the peer for the current session.
	banList   map[string]bool
	banListMu sync.RWMutex
	// aliasToDeviceID maps a banned peer's alias to their device ID, so that
	// /unban <alias> can find and remove the corresponding device:<id> entry.
	aliasToDeviceID map[string]string
}

type Room struct {
	name           string
	hasAuth        bool      // true when room was created with a password (auth required)
	keys           *RoomKeys // E2EE keys: EncKey/HMACKey from HKDF(ownerAuthKey, roomSecret), AuthKey from owner
	ownerAuthKey   []byte    // authKey of the room creator (for authenticated rooms)
	roomSecret     []byte    // random secret used in HKDF salt; rotated on kick/ban
	ownerAddr      string    // address of the room owner (first joiner); empty if room is empty
	peers          map[string]*Peer
	peersMu        sync.RWMutex
	createdLocally bool // true when the local server created this room (via CreateRoom)
}

type Peer struct {
	addr     string
	alias    string
	conn     PeerConnection
	room     *Room
	deviceID string
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
		banList:         make(map[string]bool),
		aliasToDeviceID: make(map[string]string),
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
		var rawParts []string
		if len(payload) > 0 {
			rawParts = strings.Split(payload, "|")
		}
		var roomName, providedPassword, deviceID string
		if len(rawParts) >= 4 {
			// JOIN:room|alias|password|deviceID
			roomName = rawParts[0]
			if rawParts[1] != "" {
				peer.alias = rawParts[1]
			}
			providedPassword = rawParts[2]
			deviceID = rawParts[3]
		} else if len(rawParts) >= 3 {
			// Could be JOIN:room|alias|deviceID (no password) or JOIN:room|alias|password (no deviceID)
			// Heuristic: if parts[2] is 64 hex chars, treat as deviceID (no password)
			if len(rawParts[2]) == 64 {
				roomName = rawParts[0]
				if rawParts[1] != "" {
					peer.alias = rawParts[1]
				}
				deviceID = rawParts[2]
			} else {
				roomName = rawParts[0]
				if rawParts[1] != "" {
					peer.alias = rawParts[1]
				}
				providedPassword = rawParts[2]
			}
		} else if len(rawParts) >= 2 {
			roomName = rawParts[0]
			if rawParts[1] != "" {
				peer.alias = rawParts[1]
			}
		} else if len(rawParts) >= 1 {
			roomName = rawParts[0]
		}

		// Store deviceID on the peer (stable across reconnections)
		if deviceID != "" {
			peer.deviceID = deviceID
		}

		// Reject banned addresses and device IDs immediately — before any key material is sent.
		s.banListMu.RLock()
		banned := s.banList[peer.addr]
		// Also check by device ID (stable across reconnections)
		if !banned && peer.deviceID != "" {
			banned = s.banList["device:"+peer.deviceID]
		}
		s.banListMu.RUnlock()
		if banned {
			peer.conn.Write([]byte("AUTH:FAILED|Banned\n"))
			peer.conn.Close()
			return
		}

		s.roomsMu.RLock()
		room, exists := s.rooms[roomName]
		s.roomsMu.RUnlock()

		// Derive keys using the room's current secret (rotated on kick/ban).
		// For a new room, joinRoom will generate the initial secret.
		var secretForKeyDerivation []byte
		if exists {
			secretForKeyDerivation = room.roomSecret
		}
		keys := DeriveRoomKeys(roomName, providedPassword, secretForKeyDerivation)

		// Only check auth if the room was created with a password.
		if exists && room.hasAuth {
			if providedPassword == "" {
				log.Printf("[Server] AUTH: %s rejected (no password provided) from room %s", peer.alias, roomName)
				peer.conn.Write([]byte("AUTH:FAILED|Room requires a password\n"))
				peer.conn.Close()
				return
			}
			if subtle.ConstantTimeCompare(room.keys.AuthKey, keys.AuthKey) != 1 {
				log.Printf("[Server] AUTH: %s rejected (wrong password) from room %s", peer.alias, roomName)
				peer.conn.Write([]byte("AUTH:FAILED|Incorrect password\n"))
				peer.conn.Close()
				return
			}
		}

		// Reject if alias is already in use in this room (case-insensitive).
		if exists {
			room.peersMu.RLock()
			aliasTaken := false
			for _, existingPeer := range room.peers {
				if strings.EqualFold(existingPeer.alias, peer.alias) {
					aliasTaken = true
					break
				}
			}
			room.peersMu.RUnlock()
			if aliasTaken {
				log.Printf("[Server] AUTH: %s rejected (alias '%s' already in use) from room %s", peer.alias, peer.alias, roomName)
				peer.conn.Write([]byte("AUTH:FAILED|Alias already in use\n"))
				peer.conn.Close()
				return
			}
		}

		s.joinRoom(peer, roomName, keys, providedPassword != "", secretForKeyDerivation)
		// Note: joinRoom already sends SECRET to the peer at server.go:478.
		// getOrCreate() now blocks until the SECRET is processed, so no extra
		// synchronous SECRET send is needed here.
		return
	}

	if strings.HasPrefix(message, "FROM:") {
		parts := strings.SplitN(strings.TrimPrefix(message, "FROM:"), "|", 2)
		if len(parts) == 2 {
			content := parts[1]
			if peer.room != nil {
				// Relay always verifies HMAC (E2EE always active).
				// Drops messages with invalid signatures.
				encParts := strings.SplitN(content, "|", 3)
				if len(encParts) != 3 {
					log.Printf("[E2EE] Relay dropped malformed encrypted message from %s", peer.alias)
					return
				}
				payload := encParts[0]
				nonce := encParts[1]
				hmacHex := encParts[2]
				// Use the alias from the message (parts[0]), not peer.alias.
				// peer.alias is set at JOIN time and becomes stale after /nick.
				unsigned := fmt.Sprintf("FROM:%s|%s|%s", parts[0], payload, nonce)
				if !peer.room.keys.Verify(unsigned, hmacHex) {
					log.Printf("[E2EE] Relay dropped message from %s: invalid HMAC", peer.alias)
					return
				}
				// Relay does NOT track nonces — peers do that per-connection.
				// Relay only verifies HMAC authenticity, then forwards.

				// Update peer.alias if the sender changed it via /nick, so
				// broadcastToRoom uses the correct alias when forwarding.
				if parts[0] != peer.alias {
					peer.alias = parts[0]
				}
				s.broadcastToRoom(peer.room, peer.addr, content)

				// Local display: decrypt
				var displayContent string
				if pt, err := peer.room.keys.Decrypt(payload); err == nil {
					displayContent = string(pt)
				} else {
					displayContent = content // fallback: show ciphertext
				}

				if s.onMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate(peer.alias, displayContent) {
					s.onMessage(peer.alias, peer.room.name, displayContent)
				}
			}
		}
		return
	}

	// SYSTEM:<message> — relay forwards as a system broadcast (cleartext).
	// Used for nick change notifications and other non-encrypted announcements.
	if strings.HasPrefix(message, "SYSTEM:") {
		msg := strings.TrimPrefix(message, "SYSTEM:")
		if peer.room != nil {
			s.broadcastToRoom(peer.room, "", msg)
		}
		// Also display locally for the relay operator. The local CLI shows
		// the system message via onMessage.
		if s.onMessage != nil && peer.room != nil && !s.connManager.IsDuplicate("System", msg) {
			s.onMessage("System", peer.room.name, msg)
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

	// KICK:<alias> — owner removes a peer from the room and rotates room keys.
	// The evicted peer cannot decrypt subsequent messages.
	if strings.HasPrefix(message, "KICK:") {
		targetAlias := strings.TrimPrefix(message, "KICK:")
		targetAlias = strings.TrimSpace(targetAlias)
		if peer.room != nil {
			s.handleKick(peer, targetAlias)
		}
		return
	}

	// BAN:<alias> — owner removes and blocks the peer; they cannot rejoin.
	// Room keys are rotated so the banned peer cannot decrypt messages after removal.
	if strings.HasPrefix(message, "BAN:") {
		targetAlias := strings.TrimPrefix(message, "BAN:")
		targetAlias = strings.TrimSpace(targetAlias)
		if peer.room != nil {
			s.handleBan(peer, targetAlias)
		}
		return
	}

	// UNBAN:<addr> — owner removes an address from the ban list.
	if strings.HasPrefix(message, "UNBAN:") {
		targetAddr := strings.TrimPrefix(message, "UNBAN:")
		targetAddr = strings.TrimSpace(targetAddr)
		if peer.room != nil {
			s.handleUnban(peer, targetAddr)
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

// CreateRoom pre-creates a room in the server's room registry. Used when a non-relay
// peer creates a room so that incoming JOINs from other peers pass through proper
// auth checks. Without this, the first JOIN determines the room's auth settings,
// which may differ from the creator's intent.
// cm and roomPassword are used to update the local ConnectionManager's
// room keys so the creator can send encrypted messages immediately.
func (s *Server) CreateRoom(roomName string, keys *RoomKeys, cm *ConnectionManager, roomPassword string) {
	log.Printf("[DEBUG] CreateRoom: roomName=%s, password=%s", roomName, roomPassword)
	log.Printf("[DEBUG] CreateRoom: keys.AuthKey=%x", keys.AuthKey)
	s.roomsMu.Lock()
	defer s.roomsMu.Unlock()
	if _, exists := s.rooms[roomName]; exists {
		log.Printf("[DEBUG] CreateRoom: room already exists, returning")
		return
	}
	roomSecret := GenerateRoomSecret()
	log.Printf("[DEBUG] CreateRoom: roomSecret=%x", roomSecret)
	room := &Room{
		name:           roomName,
		hasAuth:        true,
		keys:           keys,
		roomSecret:     roomSecret,
		ownerAddr:      "",
		peers:          make(map[string]*Peer),
		createdLocally: true,
	}
	room.ownerAuthKey = keys.AuthKey
	log.Printf("[DEBUG] CreateRoom: room.ownerAuthKey=%x", room.ownerAuthKey)
	encReader := hkdf.New(sha256.New, room.ownerAuthKey, room.roomSecret, []byte("p2p-messenger-enc"))
	encKey := make([]byte, 32)
	io.ReadFull(encReader, encKey)
	log.Printf("[DEBUG] CreateRoom: encKey=%x", encKey)
	hmacReader := hkdf.New(sha256.New, room.ownerAuthKey, room.roomSecret, []byte("p2p-messenger-hmac"))
	hmacKey := make([]byte, 32)
	io.ReadFull(hmacReader, hmacKey)
	log.Printf("[DEBUG] CreateRoom: hmacKey=%x", hmacKey)
	room.keys = &RoomKeys{
		AuthKey: room.ownerAuthKey,
		EncKey:  encKey,
		HMACKey: hmacKey,
	}
	s.rooms[roomName] = room
	if cm != nil {
		log.Printf("[DEBUG] CreateRoom: calling cm.UpdateRoomKeys")
		cm.UpdateRoomKeys(roomName, roomPassword, hex.EncodeToString(roomSecret))
	}
	log.Printf("[Server] Pre-created authenticated room '%s'", roomName)
}

func (s *Server) joinRoom(peer *Peer, roomName string, keys *RoomKeys, hasAuth bool, prevSecret []byte) {
	s.roomsMu.Lock()
	room, exists := s.rooms[roomName]
	if !exists {
		room = &Room{
			name:       roomName,
			hasAuth:    hasAuth,
			keys:       keys,
			roomSecret: GenerateRoomSecret(), // new room gets a fresh secret
			ownerAddr:  peer.addr,            // first joiner is the owner
			peers:      make(map[string]*Peer),
		}
		s.rooms[roomName] = room
		if hasAuth {
			room.ownerAuthKey = keys.AuthKey // preserve owner's authKey
			// Derive EncKey and HMACKey from HKDF(ownerAuthKey, roomSecret)
			encReader := hkdf.New(sha256.New, room.ownerAuthKey, room.roomSecret, []byte("p2p-messenger-enc"))
			encKey := make([]byte, 32)
			io.ReadFull(encReader, encKey)
			hmacReader := hkdf.New(sha256.New, room.ownerAuthKey, room.roomSecret, []byte("p2p-messenger-hmac"))
			hmacKey := make([]byte, 32)
			io.ReadFull(hmacReader, hmacKey)
			room.keys = &RoomKeys{
				AuthKey: room.ownerAuthKey, // owner's authKey
				EncKey:  encKey,
				HMACKey: hmacKey,
			}
		} else {
			// Unauthenticated room: re-derive with roomName-derived authKey
			room.keys = DeriveRoomKeys(roomName, "", room.roomSecret)
		}
	}

	s.roomsMu.Unlock()

	room.peersMu.Lock()
	room.peers[peer.addr] = peer
	peer.room = room
	// Transfer ownership if the previous owner has left.
	if room.ownerAddr == "" {
		room.ownerAddr = peer.addr
	}
	room.peersMu.Unlock()

	secretHex := hex.EncodeToString(room.roomSecret)
	if _, err := peer.conn.Write([]byte(fmt.Sprintf("SECRET:%s\n", secretHex))); err != nil {
		log.Printf("[Server] Failed to send room secret to %s: %v", peer.alias, err)
	}

	joinMsg := fmt.Sprintf("%s (%s) joined the chat", peer.alias, peer.addr)
	s.broadcastToRoom(room, peer.addr, joinMsg)

	if s.onSystemMessage != nil && s.connManager != nil && !s.connManager.IsDuplicate("System", joinMsg) {
		s.onSystemMessage(joinMsg)
	}

	log.Printf("[Server] Peer %s (%s) joined room '%s' (owner: %s)", peer.alias, peer.addr, roomName, room.ownerAddr)
}

func (s *Server) rotateRoomSecret(room *Room) {
	room.peersMu.Lock()
	room.roomSecret = GenerateRoomSecret()
	var authKey []byte
	if room.hasAuth && len(room.ownerAuthKey) > 0 {
		authKey = room.ownerAuthKey
	} else {
		authKey = argon2.IDKey([]byte(room.name), []byte(room.name), 1, 64*1024, 4, 32)
	}
	encReader := hkdf.New(sha256.New, authKey, room.roomSecret, []byte("p2p-messenger-enc"))
	encKey := make([]byte, 32)
	io.ReadFull(encReader, encKey)
	hmacReader := hkdf.New(sha256.New, authKey, room.roomSecret, []byte("p2p-messenger-hmac"))
	hmacKey := make([]byte, 32)
	io.ReadFull(hmacReader, hmacKey)
	room.keys = &RoomKeys{
		AuthKey: authKey,
		EncKey:  encKey,
		HMACKey: hmacKey,
	}
	remainingPeers := make([]*Peer, 0, len(room.peers))
	for _, p := range room.peers {
		remainingPeers = append(remainingPeers, p)
	}
	room.peersMu.Unlock()
	newSecretHex := hex.EncodeToString(room.roomSecret)
	secretMsg := fmt.Sprintf("SECRET:%s\n", newSecretHex)
	notified := 0
	for _, p := range remainingPeers {
		if _, err := p.conn.Write([]byte(secretMsg)); err != nil {
			log.Printf("[Server] rotateRoomSecret: write error for %s (addr=%s): %v", p.alias, p.addr, err)
			continue
		}
		notified++
	}
	log.Printf("[Server] Room '%s' keys rotated — %d peer(s) notified", room.name, notified)

	if s.connManager != nil {
		if err := s.connManager.UpdateRoomKeys(room.name, "", newSecretHex); err != nil {
			log.Printf("[Server] rotateRoomSecret: failed to update local CM keys: %v", err)
		} else {
			log.Printf("[Server] Room '%s' — local CM keys rotated", room.name)
		}
	}
}

// handleKick removes a peer from the room and rotates room keys so the evicted
// peer cannot decrypt subsequent messages, only the room owner can kick.
func (s *Server) handleKick(requester *Peer, targetAlias string) {
	room := requester.room

	room.peersMu.RLock()
	isOwner := room.ownerAddr == requester.addr || requester.addr == "creator"
	room.peersMu.RUnlock()

	if !isOwner {
		log.Printf("[Server] %s attempted KICK but is not the room owner", requester.alias)
		return
	}

	room.peersMu.Lock()
	var targetPeer *Peer
	var targetAddr string
	for addr, p := range room.peers {
		if p.alias == targetAlias {
			targetPeer = p
			targetAddr = addr
			break
		}
	}
	if targetPeer == nil {
		room.peersMu.Unlock()
		log.Printf("[Server] KICK: no peer named '%s' in room '%s'", targetAlias, room.name)
		return
	}
	delete(room.peers, targetAddr)
	room.peersMu.Unlock()

	kickMsg := fmt.Sprintf("SYSTEM:You were kicked from room '%s' by the owner\n", room.name)
	if _, err := targetPeer.conn.Write([]byte(kickMsg)); err != nil {
		log.Printf("[Server] kick write error: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	s.rotateRoomSecret(room)
	targetPeer.conn.Close()
	if s.connManager != nil {
		s.connManager.removeConnection(targetAddr)
	}

	leaveMsg := fmt.Sprintf("%s was removed from the chat by the owner", targetAlias)
	s.broadcastToRoom(room, "", leaveMsg)
	if s.onSystemMessage != nil {
		s.onSystemMessage(leaveMsg)
	}
	log.Printf("[Server] %s kicked %s from room '%s'", requester.alias, targetAlias, room.name)
}

// handleBan removes a peer, adds their address to the ban list, and rotates room keys.
// The banned peer cannot rejoin. Only the room owner can ban.
func (s *Server) handleBan(requester *Peer, targetAlias string) {
	room := requester.room

	room.peersMu.RLock()
	isOwner := room.ownerAddr == requester.addr || requester.addr == "creator"
	room.peersMu.RUnlock()

	if !isOwner {
		log.Printf("[Server] %s attempted BAN but is not the room owner", requester.alias)
		return
	}

	room.peersMu.Lock()
	var targetPeer *Peer
	var targetAddr string
	for addr, p := range room.peers {
		if p.alias == targetAlias {
			targetPeer = p
			targetAddr = addr
			break
		}
	}
	if targetPeer == nil {
		room.peersMu.Unlock()
		log.Printf("[Server] BAN: no peer named '%s' in room '%s'", targetAlias, room.name)
		return
	}
	delete(room.peers, targetAddr)
	room.peersMu.Unlock()

	// Add to ban list BEFORE rotating keys — banned peer must not receive new secret.
	s.banListMu.Lock()
	s.banList[targetAddr] = true
	if targetPeer.deviceID != "" {
		s.banList["device:"+targetPeer.deviceID] = true
	}
	s.banList["alias:"+targetPeer.alias] = true
	s.aliasToDeviceID[targetPeer.alias] = targetPeer.deviceID
	s.banListMu.Unlock()

	// Send BANNED notification to the evicted peer BEFORE closing the connection.
	// This gives them a clear message about why they were disconnected.
	banMsg := fmt.Sprintf("SYSTEM:You were banned from room '%s' by the owner\n", room.name)
	if _, err := targetPeer.conn.Write([]byte(banMsg)); err != nil {
		log.Printf("[Server] ban write error: %v", err)
	}

	// Give the banned peer a moment to receive the notification before closing
	time.Sleep(200 * time.Millisecond)

	// Rotate keys AFTER adding to ban list.
	s.rotateRoomSecret(room)

	// Close the banned peer's connection.
	targetPeer.conn.Close()
	if s.connManager != nil {
		s.connManager.removeConnection(targetAddr)
	}

	leaveMsg := fmt.Sprintf("%s was banned from the chat by the owner", targetAlias)
	s.broadcastToRoom(room, "", leaveMsg)
	if s.onSystemMessage != nil {
		s.onSystemMessage(leaveMsg)
	}
	log.Printf("[Server] %s banned %s (%s) from room '%s'", requester.alias, targetAlias, targetAddr, room.name)
}

func (s *Server) handleUnban(requester *Peer, target string) {
	room := requester.room
	if room == nil {
		s.roomsMu.RLock()
		for _, r := range s.rooms {
			room = r
			break
		}
		s.roomsMu.RUnlock()
		if room == nil {
			log.Printf("[Server] %s attempted UNBAN but no rooms exist", requester.alias)
			return
		}
	}

	room.peersMu.RLock()
	isOwner := room.ownerAddr == requester.addr || requester.addr == "creator"
	room.peersMu.RUnlock()

	if !isOwner {
		log.Printf("[Server] %s attempted UNBAN but is not the room owner", requester.alias)
		return
	}

	s.banListMu.Lock()
	removedCount := 0
	if _, ok := s.banList[target]; ok {
		delete(s.banList, target)
		removedCount++
	}
	//    the device:<id> entry via the reverse mapping.
	if !strings.HasPrefix(target, "device:") && !strings.HasPrefix(target, "alias:") {
		// Remove alias: entry if present
		if _, ok := s.banList["alias:"+target]; ok {
			delete(s.banList, "alias:"+target)
			removedCount++
		}
		// Look up device ID from reverse map and remove device: entry
		if deviceID, ok := s.aliasToDeviceID[target]; ok && deviceID != "" {
			if _, ok := s.banList["device:"+deviceID]; ok {
				delete(s.banList, "device:"+deviceID)
				removedCount++
			}
			delete(s.aliasToDeviceID, target)
		}
	}

	// 3. If target has explicit alias: prefix, strip and treat like bare alias
	if strings.HasPrefix(target, "alias:") {
		alias := strings.TrimPrefix(target, "alias:")
		if deviceID, ok := s.aliasToDeviceID[alias]; ok && deviceID != "" {
			if _, ok := s.banList["device:"+deviceID]; ok {
				delete(s.banList, "device:"+deviceID)
				removedCount++
			}
			delete(s.aliasToDeviceID, alias)
		}
	}

	// 4. If target has explicit device: prefix, clean up the corresponding alias entry
	if strings.HasPrefix(target, "device:") {
		deviceID := strings.TrimPrefix(target, "device:")
		for alias, did := range s.aliasToDeviceID {
			if did == deviceID {
				if _, ok := s.banList["alias:"+alias]; ok {
					delete(s.banList, "alias:"+alias)
					removedCount++
				}
				delete(s.aliasToDeviceID, alias)
			}
		}
	}

	s.banListMu.Unlock()

	log.Printf("[Server] %s unbanned %s (removed %d ban entries)", requester.alias, target, removedCount)
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

	// Enqueue the message onto each peer's dedicated outbound writer instead of
	// writing inline. This is the critical fix for high-peer-count collapse:
	// broadcastToRoom runs on the *sender's* per-peer reader goroutine, so the
	// previous "spawn a goroutine per peer and wg.Wait()" approach blocked that
	// reader for the whole fan-out. While blocked it could not process incoming
	// PONGs, so the health check tore down peers that were actually alive,
	// cascading into total delivery failure at n>=500. Enqueueing is O(1) and
	// never blocks: a slow peer only backs up (and ultimately drops from) its
	// own bounded queue. Per-connection writes are serialised by the writer
	// goroutine, which also removes the unsafe concurrent QUIC stream writes.
	//
	// Performance: take the connection-map read lock ONCE for the whole fan-out
	// instead of once per peer. At high peer counts (n~1000) the per-peer
	// RLock/RUnlock cycle meant ~999 lock acquisitions per broadcast on the hot
	// path; a single RLock around the non-blocking enqueue loop removes that
	// churn and the associated contention with connection setup/teardown. Peers
	// without a managed connection (e.g. unit tests) are collected and written
	// outside the lock so a slow socket still cannot stall the loop. Drops are
	// counted and logged after the lock is released (no I/O under the lock).
	var fallback []*Peer
	dropped := 0
	if s.connManager != nil {
		s.connManager.mu.RLock()
		for _, peer := range peers {
			if mc, ok := s.connManager.connections[peer.addr]; ok {
				if !mc.enqueue(msgBytes) {
					dropped++
				}
			} else {
				fallback = append(fallback, peer)
			}
		}
		s.connManager.mu.RUnlock()
	} else {
		fallback = peers
	}

	if dropped > 0 {
		log.Printf("[Server] Broadcast queue full for %d peer(s), dropped message", dropped)
	}

	for _, peer := range fallback {
		p := peer
		go func() {
			if _, err := p.conn.Write(msgBytes); err != nil {
				log.Printf("[Server] Broadcast error to %s: %v", p.addr, err)
			}
		}()
	}
}

func (s *Server) removePeer(peer *Peer) {
	if peer.room != nil {
		var roomName string
		peer.room.peersMu.Lock()
		wasOwner := peer.room.ownerAddr == peer.addr
		delete(peer.room.peers, peer.addr)
		empty := len(peer.room.peers) == 0
		// Transfer ownership to the remaining peer with the earliest address (deterministic).
		if wasOwner && !empty {
			var newOwnerAddr string
			for addr := range peer.room.peers {
				if newOwnerAddr == "" || addr < newOwnerAddr {
					newOwnerAddr = addr
				}
			}
			peer.room.ownerAddr = newOwnerAddr
		} else if empty {
			peer.room.ownerAddr = ""
			roomName = peer.room.name
		}
		peer.room.peersMu.Unlock()

		if empty && roomName != "" {
			s.roomsMu.Lock()
			if room, exists := s.rooms[roomName]; exists {
				// Don't delete rooms that were created locally (by this server's
				// owner via CreateRoom). These rooms stay in the registry so that
				// /unban and other owner commands can still find them even when
				// no peers are connected.
				if room.createdLocally {
					s.roomsMu.Unlock()
					goto skipRoomDeletion
				}
				room.peersMu.RLock()
				stillEmpty := len(room.peers) == 0
				room.peersMu.RUnlock()
				if stillEmpty {
					delete(s.rooms, roomName)
				}
			}
			s.roomsMu.Unlock()
		}
	skipRoomDeletion:

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
	// Send a clear "server shutting down" notification to all connected peers
	// BEFORE closing connections. This way peers learn immediately that the
	// server is going away, without having to type a command to detect EOF.
	shutdownMsg := "SYSTEM:Server is shutting down\n"
	s.roomsMu.RLock()
	type peerTarget struct {
		peer *Peer
	}
	var targets []peerTarget
	for _, room := range s.rooms {
		room.peersMu.RLock()
		for _, p := range room.peers {
			targets = append(targets, peerTarget{peer: p})
		}
		room.peersMu.RUnlock()
	}
	s.roomsMu.RUnlock()
	for _, t := range targets {
		if _, err := t.peer.conn.Write([]byte(shutdownMsg)); err != nil {
			log.Printf("[Server] Close: write error to %s: %v", t.peer.alias, err)
		}
	}
	// Give the message time to be flushed and delivered to peers.
	time.Sleep(200 * time.Millisecond)

	// Now do the existing cleanup: remove all peers (with their leave messages).
	done := make(chan struct{})
	go func() {
		s.roomsMu.RLock()
		for _, room := range s.rooms {
			room.peersMu.RLock()
			peers := make([]*Peer, 0, len(room.peers))
			for _, p := range room.peers {
				peers = append(peers, p)
			}
			room.peersMu.RUnlock()
			for _, p := range peers {
				s.removePeer(p)
			}
		}
		s.roomsMu.RUnlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

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
