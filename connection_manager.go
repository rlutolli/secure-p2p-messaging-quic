/*
connection_manager.go - Outgoing Connection Pool and Message Deduplication
*/
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

var (
	ErrBanned = errors.New("banned from room")
	ErrWrongPassword = errors.New("wrong password")
	ErrTimeout = errors.New("connection timed out")
)

// authError carries one of the sentinel errors plus the reason string
// the relay sent in AUTH:FAILED|<reason>.
type authError struct {
	kind   error
	reason string
}

func (e *authError) Error() string {
	return e.kind.Error() + ": " + e.reason
}

func (e *authError) Unwrap() error {
	return e.kind
}

func (e *authError) Is(target error) bool {
	return errors.Is(e.kind, target)
}

type ConnectionManager struct {
	mu           sync.RWMutex
	connections  map[string]*ManagedConnection
	localPort    int
	roomName     string
	localAlias   string
	deviceID     string
	useTCP       bool
	roomPassword string
	// raceTransports, when true, dials QUIC and TCP in parallel and keeps the
	// first to complete its handshake (Happy-Eyeballs style): min(QUIC,TCP) setup
	// latency plus automatic fallback if one transport is blocked. Off by default.
	raceTransports bool

	roomKeys   *RoomKeys
	roomSecret []byte

	sessionCache tls.ClientSessionCache

	seenMsgsMu sync.Mutex
	seenMsgs   map[string]time.Time

	// TOFU: stores the SHA-256 fingerprint of each peer's certificate on first connect.
	// Reconnections that present a different certificate are rejected.
	tofuMu           sync.RWMutex
	tofuFingerprints map[string]string
	tofuPath string

	pendingPingsMu sync.Mutex
	pendingPings   map[string]time.Time

	relayAddrs map[string]bool
	relayMu    sync.RWMutex

	// kicked is closed when this peer is kicked or banned from a room.
	kicked chan struct{}
	// disconnected is closed when the connection to the relay/server is lost.
	disconnected chan struct{}
}

type ManagedConnection struct {
	conn      PeerConnection
	peerAddr  string
	createdAt time.Time
	lastUsed  time.Time
	mu        sync.Mutex

	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once

	replay *ReplayProtector // per-connection replay nonce tracking (nil when E2EE off)

	// secretReady blocks getOrCreate() until the SECRET message is processed
	// by readLoop, ensuring room keys are ready before Send() is called.
	secretReady    sync.WaitGroup
	secretDoneOnce sync.Once // ensures Done() is called exactly once even if multiple SECRET messages arrive

	lastAuthErr error // populated by readLoop when AUTH:FAILED is received
}

// newManagedConnection builds a ManagedConnection, the connection is registered.
func newManagedConnection(conn PeerConnection, peerAddr string) *ManagedConnection {
	return &ManagedConnection{
		conn:      conn,
		peerAddr:  peerAddr,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
		out:       make(chan []byte, OutboundQueueSize),
		done:      make(chan struct{}),
	}
}

func (mc *ManagedConnection) enqueue(msg []byte) bool {
	select {
	case <-mc.done:
		return false
	default:
	}
	select {
	case mc.out <- msg:
		return true
	default:
		return false
	}
}

func (mc *ManagedConnection) signalClosed() {
	if mc.done == nil {
		return
	}
	mc.closeOnce.Do(func() { close(mc.done) })
}

func NewConnectionManager(localPort int, roomName string, useTCP bool, roomPassword string, isRelay bool, explicitAlias string) *ConnectionManager {
	deviceID, _ := GetDeviceID()
	var alias string
	if explicitAlias != "" {
		alias = explicitAlias
	} else {
		alias = LoadAlias(deviceID)
		if alias == "" {
			alias = generateAlias()
			_ = SaveAlias(deviceID, alias) // best effort, ignore error
		}
	}
	cm := &ConnectionManager{
		connections:      make(map[string]*ManagedConnection),
		localPort:        localPort,
		roomName:         roomName,
		localAlias:       alias,
		deviceID:         deviceID,
		seenMsgs:         make(map[string]time.Time),
		useTCP:           useTCP,
		roomPassword:     roomPassword,
		sessionCache:     tls.NewLRUClientSessionCache(100),
		tofuFingerprints: make(map[string]string),
		pendingPings:     make(map[string]time.Time),
		relayAddrs:       make(map[string]bool),
		kicked:           make(chan struct{}),
		disconnected:     make(chan struct{}),
	}

	cm.roomKeys = DeriveRoomKeys(roomName, roomPassword, nil) // nil secret; updated on SECRET: receipt

	go cm.cleanupSeenMsgs()
	go cm.healthCheck()

	return cm
}

func (cm *ConnectionManager) UpdateRoomKeys(roomName, roomPassword string, secretHex string) error {
	log.Printf("[DEBUG] UpdateRoomKeys: roomName=%s, password=%s, secretHex=%s", roomName, roomPassword, secretHex)
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		return fmt.Errorf("UpdateRoomKeys: invalid hex secret: %w", err)
	}
	log.Printf("[DEBUG] UpdateRoomKeys: secret=%x", secret)
	cm.mu.Lock()
	cm.roomSecret = secret
	var authKey []byte
	if roomPassword != "" {
		authKey = argon2.IDKey([]byte(roomPassword), []byte(roomName), 1, 64*1024, 4, 32)
		log.Printf("[DEBUG] UpdateRoomKeys: authKey (from password)=%x", authKey)
	} else {
		authKey = cm.roomKeys.AuthKey // nil-derived, unchanged
		log.Printf("[DEBUG] UpdateRoomKeys: authKey (unchanged)=%x", authKey)
	}
	encReader := hkdf.New(sha256.New, authKey, secret, []byte("p2p-messenger-enc"))
	encKey := make([]byte, 32)
	io.ReadFull(encReader, encKey)
	log.Printf("[DEBUG] UpdateRoomKeys: encKey=%x", encKey)
	hmacReader := hkdf.New(sha256.New, authKey, secret, []byte("p2p-messenger-hmac"))
	hmacKey := make([]byte, 32)
	io.ReadFull(hmacReader, hmacKey)
	log.Printf("[DEBUG] UpdateRoomKeys: hmacKey=%x", hmacKey)
	cm.roomKeys = &RoomKeys{
		AuthKey: authKey,
		EncKey:  encKey,
		HMACKey: hmacKey,
	}
	cm.mu.Unlock()
	log.Printf("[E2EE] Room keys rotated — new secret active")
	return nil
}

func (cm *ConnectionManager) WaitForRoomKeysUpdate(previousSecret []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cm.mu.RLock()
		secret := cm.roomSecret
		cm.mu.RUnlock()
		// Wait until secret is non-nil AND different from what we knew before.
		if secret != nil && (previousSecret == nil || string(secret) != string(previousSecret)) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (cm *ConnectionManager) checkAndPin(peerAddr, fpHex string) error {
	cm.tofuMu.Lock()
	known, ok := cm.tofuFingerprints[peerAddr]
	if !ok {
		cm.tofuFingerprints[peerAddr] = fpHex
	}
	cm.tofuMu.Unlock()

	if ok {
		if known != fpHex {
			return fmt.Errorf("TOFU: certificate fingerprint mismatch for %s (possible MITM): pinned %s, got %s",
				peerAddr, known, fpHex)
		}
		return nil
	}

	// First contact for this address. Persist the new pin if a store is set.
	if cm.tofuPath != "" {
		if err := appendKnownPeer(cm.tofuPath, peerAddr, fpHex); err != nil {
			log.Printf("[TOFU] warning: could not persist pin for %s: %v", peerAddr, err)
		}
	}
	return nil
}

func (cm *ConnectionManager) LoadKnownPeers(path string) error {
	cm.tofuPath = path

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	cm.tofuMu.Lock()
	defer cm.tofuMu.Unlock()

	loaded := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			cm.tofuFingerprints[parts[0]] = parts[1]
			loaded++
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	log.Printf("[TOFU] loaded %d pinned peer(s) from %s", loaded, path)
	return nil
}

// knownPeersWriteMu serialises appends to the known-peers file across goroutines.
var knownPeersWriteMu sync.Mutex

// appendKnownPeer appends a single "addr fingerprint" line to the store.
func appendKnownPeer(path, addr, fpHex string) error {
	knownPeersWriteMu.Lock()
	defer knownPeersWriteMu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s\n", addr, fpHex)
	return err
}

// dialTCPPeer establishes a TCP+TLS connection to peerAddr.
func (cm *ConnectionManager) dialTCPPeer(ctx context.Context, peerAddr string, tlsConf *tls.Config) (PeerConnection, error) {
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
	return &NetConnWrapper{Conn: tlsConn}, nil
}

// dialQUICPeer establishes a QUIC connection (with the tuned transport config)
// to peerAddr and opens the first stream.
func (cm *ConnectionManager) dialQUICPeer(ctx context.Context, peerAddr string, tlsConf *tls.Config) (PeerConnection, error) {
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
	return &QuicConnectionWrapper{Stream: stream, Conn: conn}, nil
}

func (cm *ConnectionManager) raceDial(ctx context.Context, peerAddr string, tlsConf *tls.Config) (PeerConnection, error) {
	type result struct {
		pc  PeerConnection
		err error
	}
	rctx, cancel := context.WithCancel(ctx)
	ch := make(chan result, 2)
	go func() { pc, e := cm.dialQUICPeer(rctx, peerAddr, tlsConf); ch <- result{pc, e} }()
	go func() { pc, e := cm.dialTCPPeer(rctx, peerAddr, tlsConf); ch <- result{pc, e} }()

	var lastErr error
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil && r.pc != nil {
			cancel()    // stop the slower transport
			go func() { // close the loser if it still completes
				if lr := <-ch; lr.pc != nil {
					lr.pc.Close()
				}
			}()
			return r.pc, nil
		}
		lastErr = r.err
	}
	cancel()
	if lastErr == nil {
		lastErr = fmt.Errorf("raceDial: both transports failed for %s", peerAddr)
	}
	return nil, fmt.Errorf("race dial failed: %w", lastErr)
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

// GetDeviceID returns the stable device ID persisted by the deviceid package.
func (cm *ConnectionManager) GetDeviceID() string {
	return cm.deviceID
}

// SetLocalAlias changes the local peer's display name. The alias is sanitized
// to prevent protocol injection (|, \n, \r are stripped).
func (cm *ConnectionManager) SetLocalAlias(alias string) {
	cm.mu.Lock()
	cm.localAlias = sanitiseField(alias)
	cm.mu.Unlock()
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
	// Double-check after acquiring write lock
	if mc, exists := cm.connections[peerAddr]; exists {
		cm.mu.Unlock()
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
			return cm.checkAndPin(peerAddr, hex.EncodeToString(fp[:]))
		},
	}

	var derr error
	switch {
	case cm.raceTransports:
		pc, derr = cm.raceDial(ctx, peerAddr, tlsConf)
	case cm.useTCP:
		pc, derr = cm.dialTCPPeer(ctx, peerAddr, tlsConf)
	default:
		pc, derr = cm.dialQUICPeer(ctx, peerAddr, tlsConf)
	}
	if derr != nil {
		cm.mu.Unlock()
		return nil, derr
	}

	deviceID, _ := GetDeviceID()
	var handshake string
	if cm.roomPassword != "" {
		if deviceID != "" {
			handshake = fmt.Sprintf("JOIN:%s|%s|%s|%s\n", cm.roomName, cm.localAlias, cm.roomPassword, deviceID)
		} else {
			handshake = fmt.Sprintf("JOIN:%s|%s|%s\n", cm.roomName, cm.localAlias, cm.roomPassword)
		}
	} else {
		if deviceID != "" {
			handshake = fmt.Sprintf("JOIN:%s|%s|%s\n", cm.roomName, cm.localAlias, deviceID)
		} else {
			handshake = fmt.Sprintf("JOIN:%s|%s\n", cm.roomName, cm.localAlias)
		}
	}
	if _, err := pc.Write([]byte(handshake)); err != nil {
		pc.Close()
		cm.mu.Unlock()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	mc = newManagedConnection(pc, peerAddr)
	mc.replay = NewReplayProtector(5 * time.Minute)
	cm.connections[peerAddr] = mc

	// Start goroutines and release the lock so readLoop can call
	// UpdateRoomKeys (which needs cm.mu.Lock) without deadlocking.
	mc.secretReady.Add(1)
	go cm.readLoop(mc)
	go cm.writeLoop(mc)
	cm.mu.Unlock()

	// Wait with timeout so wrong passwords / network failures don't hang.
	secretDone := make(chan struct{})
	go func() {
		mc.secretReady.Wait()
		close(secretDone)
	}()
	select {
	case <-secretDone:
		// SECRET or AUTH:FAILED received. Check whether the connection
		// survived — AUTH:FAILED calls removeConnection which deletes it.
		cm.mu.RLock()
		_, survived := cm.connections[peerAddr]
		cm.mu.RUnlock()
		if !survived {
			// Check if the readLoop captured an AUTH:FAILED reason.
			mc.mu.Lock()
			err := mc.lastAuthErr
			mc.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, &authError{kind: ErrWrongPassword, reason: "no response from relay"}
		}
		return mc, nil
	case <-time.After(5 * time.Second):
		cm.removeConnection(peerAddr)
		return nil, &authError{kind: ErrTimeout, reason: "no response from relay within 5s"}
	}
}

func (cm *ConnectionManager) writeLoop(mc *ManagedConnection) {
	for {
		select {
		case <-mc.done:
			return
		case msg := <-mc.out:
			batch := make([]byte, 0, len(msg)*2)
			batch = append(batch, msg...)
		coalesce:
			for {
				select {
				case more := <-mc.out:
					batch = append(batch, more...)
				default:
					break coalesce
				}
			}
			mc.mu.Lock()
			_, err := mc.conn.Write(batch)
			mc.mu.Unlock()
			if err != nil {
				cm.removeConnection(mc.peerAddr)
				return
			}
		}
	}
}

func (cm *ConnectionManager) Enqueue(peerAddr string, msg []byte) bool {
	cm.mu.RLock()
	mc, ok := cm.connections[peerAddr]
	cm.mu.RUnlock()
	if !ok {
		return false
	}
	return mc.enqueue(msg)
}

func (cm *ConnectionManager) readLoop(mc *ManagedConnection) {
	defer cm.removeConnection(mc.peerAddr)

	reader := bufio.NewReader(mc.conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			select {
			case <-cm.kicked:
				// kick path already handled
			case <-cm.disconnected:
				// already closed
			default:
				close(cm.disconnected)
			}
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
			// Categorize the reason into one of our sentinel errors.
			mc.mu.Lock()
			if strings.Contains(strings.ToLower(reason), "banned") {
				mc.lastAuthErr = &authError{kind: ErrBanned, reason: reason}
			} else {
				mc.lastAuthErr = &authError{kind: ErrWrongPassword, reason: reason}
			}
			mc.mu.Unlock()
			cm.removeConnection(mc.peerAddr)
			mc.secretDoneOnce.Do(mc.secretReady.Done)
			return
		}

		// server sends the current room secret (hex encoded) after a key rotation.
		// The client re-derives room keys with the new secret so subsequent messages
		// use the updated encryption. Peers who were kicked/banned do not receive this
		// message and cannot decrypt new messages.
		if strings.HasPrefix(line, "SECRET:") {
			secretHex := strings.TrimPrefix(line, "SECRET:")
			secretHex = strings.TrimSpace(secretHex)
			if secretHex != "" {
				cm.mu.RLock()
				roomName := cm.roomName
				roomPassword := cm.roomPassword
				cm.mu.RUnlock()

				if err := cm.UpdateRoomKeys(roomName, roomPassword, secretHex); err != nil {
					log.Printf("[E2EE] Failed to update room keys: %v", err)
				} else {
					fmt.Printf("\n%s\n> ", formatSystemMessage("Room security keys have been updated"))
				}
			}
			// Use Once to handle the server's double-SECRET for new authenticated rooms.
			// getOrCreate() must not block forever even if keys failed to update.
			mc.secretDoneOnce.Do(mc.secretReady.Done)
			continue
		}

		var formatted string
		var sender, message string

		if strings.HasPrefix(line, "SYSTEM:") {
			message = strings.TrimPrefix(line, "SYSTEM:")
			sender = "System"

			// Detect kick/ban notifications to self, so the CLI loop
			// can return to room selection.
			msgLower := strings.ToLower(message)
			if (strings.Contains(msgLower, "kicked") || strings.Contains(msgLower, "banned")) &&
				strings.Contains(msgLower, "you were") {
				select {
				case <-cm.kicked:
					// already closed
				default:
					close(cm.kicked)
				}
			}

			if cm.IsDuplicate(sender, message) {
				continue
			}
			formatted = formatSystemMessage(message)
		} else if strings.HasPrefix(line, "FROM:") {
			raw := strings.TrimPrefix(line, "FROM:")
			parts := strings.SplitN(raw, "|", 4)

			if len(parts) == 4 {
				// Encrypted + HMAC-signed message (E2EE always active)
				sender = parts[0]
				payload := parts[1]
				nonce := parts[2]
				hmacHex := parts[3]

				// Verify HMAC
				unsigned := fmt.Sprintf("FROM:%s|%s|%s", sender, payload, nonce)
				log.Printf("[DEBUG] HMAC verify: unsigned=%s", unsigned)
				log.Printf("[DEBUG] HMAC verify: cm.roomKeys.HMACKey=%x", cm.roomKeys.HMACKey)
				log.Printf("[DEBUG] HMAC verify: received hmacHex=%s", hmacHex)
				expectedHMAC := cm.roomKeys.Sign(unsigned)
				log.Printf("[DEBUG] HMAC verify: expected hmacHex=%s", expectedHMAC)
				if !cm.roomKeys.Verify(unsigned, hmacHex) {
					log.Printf("[E2EE] HMAC verification failed from %s", sender)
					continue
				}

				// Check replay
				if !mc.replay.Check(nonce) {
					log.Printf("[E2EE] Replay detected from %s", sender)
					continue
				}

				// Decrypt
				plaintext, err := cm.roomKeys.Decrypt(payload)
				if err != nil {
					log.Printf("[E2EE] Decryption failed from %s: %v", sender, err)
					continue
				}
				message = string(plaintext)

				if cm.IsDuplicate(sender, message) {
					continue
				}
				formatted = formatMessage(sender, message)
			} else {
				// Malformed or legacy FROM message — drop
				log.Printf("[E2EE] Dropped non-E2EE FROM message from %s", mc.peerAddr)
				continue
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
		mc.signalClosed()
		if mc.replay != nil {
			mc.replay.Close()
		}
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

	payload, err := cm.roomKeys.Encrypt([]byte(msg))
	if err != nil {
		return fmt.Errorf("Send: encrypt: %w", err)
	}
	nonce := generateReplayNonce()
	unsigned := fmt.Sprintf("FROM:%s|%s|%s", alias, payload, nonce)
	sig := cm.roomKeys.Sign(unsigned)
	formatted := unsigned + "|" + sig + "\n"
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

	mc := newManagedConnection(pc, peerAddr)
	mc.replay = NewReplayProtector(5 * time.Minute)

	cm.connections[peerAddr] = mc

	if cm.roomKeys != nil {
		mc.replay = NewReplayProtector(5 * time.Minute)
	}

	go cm.writeLoop(mc)
}

func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for addr, mc := range cm.connections {
		mc.signalClosed()
		mc.conn.Close()
		delete(cm.connections, addr)
	}
}

// generateReplayNonce creates a random 8-byte nonce for replay protection.
func generateReplayNonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SendRaw writes a raw line to a connected peer. Used for system notifications
// like alias changes. Does NOT encrypt — only use for cleartext SYSTEM messages.
func (cm *ConnectionManager) SendRaw(peerAddr string, line string) error {
	cm.mu.RLock()
	mc, exists := cm.connections[peerAddr]
	cm.mu.RUnlock()
	if !exists {
		return fmt.Errorf("no connection to %s", peerAddr)
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	_, err := mc.conn.Write([]byte(line))
	return err
}
