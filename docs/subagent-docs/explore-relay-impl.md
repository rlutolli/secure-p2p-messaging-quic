# Relay Peer Architecture Exploration

## Overview

The current architecture is a **full-mesh P2P** model where every peer connects directly to every other peer in a room. There is no central relay — the "server" in each instance is just a listener that accepts incoming connections and manages local room state. To implement a relay mode, the room creator would need to become a hub that routes messages between peers who are NOT directly connected to each other.

---

## Files

| File | Description |
|------|-------------|
| `main.go` | App entry point, CLI, room creation/joining flow, `sendToRoom()` broadcast |
| `server.go` | QUIC+TCP listener, `handlePeerConnection`, room management, `broadcastToRoom` |
| `connection_manager.go` | Outgoing connection pool, `getOrCreate`, `Send`, `readLoop`, health checks |
| `transport.go` | `PeerConnection` interface, `QuicConnectionWrapper`, `NetConnWrapper` |
| `discovery.go` | UDP broadcast-based LAN peer discovery, `DiscoveryService` |
| `rendezvous.go` | HTTP rendezvous client for WAN peer registration/lookup |
| `security.go` | TLS cert generation (self-signed, ephemeral), argon2id room key derivation |
| `performance.go` | QUIC config constants (timeouts, windows, stream limits) |
| `helpers.go` | Alias generation, field sanitisation |

---

## Key Symbols

### server.go

```
server.go:31  — type Server struct
  Fields: quicListener, tcpListener, rooms (map[string]*Room), connManager, onMessage, onSystemMessage

server.go:42  — type Room struct
  Fields: name, passwordHash, peers (map[string]*Peer), peersMu

server.go:49  — type Peer struct
  Fields: addr (string), alias (string), conn (PeerConnection), room (*Room)

server.go:56  — func NewServer(addr, onMessage, onSystemMessage, connManager) (*Server, error)
  Creates QUIC listener on given addr, TCP listener on same port, starts accept loops.
  Called by: main.go:292 (initializeApp)

server.go:154 — func (s *Server) handlePeerConnection(pc PeerConnection, peerAddr string)
  THE CORE HANDLER. Called for every incoming connection (QUIC or TCP).
  1. Registers incoming connection in ConnectionManager via RegisterIncoming()
  2. Creates a Peer struct with auto-generated alias
  3. Enters read loop: reads \n-delimited lines -> handleMessage()
  4. On error/EOF -> removePeer()
  Called by: handleQUICConnection (line 135), handleTCPConnection (line 151)

server.go:185 — func (s *Server) handleMessage(peer *Peer, message string)
  Message router. Handles:
  - "PING" -> writes "PONG"
  - "PONG" -> clears ping in ConnectionManager
  - "JOIN:<roomName>|<alias>|<password>" -> joinRoom()
  - "FROM:<alias>|<message>" -> broadcastToRoom() + onMessage callback
  - "MSG:<message>" -> broadcastToRoom() + onMessage callback (legacy)
  - plain text -> broadcastToRoom() + onMessage callback (if peer in room)
  Called by: handlePeerConnection read loop (line 181)

server.go:264 — func (s *Server) joinRoom(peer *Peer, roomName string, passwordHash string)
  Creates room if doesn't exist, adds peer to room.peers map, broadcasts join notification.
  Password logic: if room exists with password, verifies via constant-time compare.
  If room doesn't exist and password provided, sets passwordHash (first peer = room creator).
  Called by: handleMessage (line 227)

server.go:292 — func (s *Server) broadcastToRoom(room *Room, senderAddr, message string)
  THE BROADCAST ENGINE. Iterates all peers in room EXCEPT sender, writes formatted message.
  Format: "SYSTEM:<msg>\n" for system, "FROM:<alias>|<msg>\n" for user messages.
  Uses sync.WaitGroup for parallel writes.
  Called by: handleMessage (lines 236, 248, 257), joinRoom (line 283), removePeer (line 361)

server.go:336 — func (s *Server) removePeer(peer *Peer)
  Removes peer from room, deletes empty rooms, broadcasts leave message, closes connection.
  Double-checks empty-room deletion under both room lock and server lock.
  Called by: handlePeerConnection on read error (line 172), handleMessage on auth failure (line 219)
```

### connection_manager.go

```
connection_manager.go:23  — type ConnectionManager struct
  Fields: connections (map[string]*ManagedConnection), localPort, roomName, localAlias,
          useTCP, roomPassword, sessionCache, seenMsgs, tofuFingerprints, pendingPings

connection_manager.go:46  — type ManagedConnection struct
  Fields: conn (PeerConnection), peerAddr, createdAt, lastUsed, mu (per-conn lock)

connection_manager.go:54  — func NewConnectionManager(localPort, roomName, useTCP, roomPassword) *ConnectionManager
  Creates CM with empty connection pool, starts cleanupSeenMsgs() and healthCheck() goroutines.
  Called by: main.go:282 (initializeApp) — note: localPort=0 initially, set later from server

connection_manager.go:170 — func (cm *ConnectionManager) GetOrCreate(ctx, peerAddr) (*ManagedConnection, error)
  Public wrapper -> calls getOrCreate()

connection_manager.go:174 — func (cm *ConnectionManager) getOrCreate(ctx, peerAddr) (*ManagedConnection, error)
  THE OUTGOING CONNECTION CREATOR. Double-checked locking pattern:
  1. RLock -> check if connection exists -> return if found
  2. Lock -> double-check -> create if still missing
  3. Dials peer (TCP+TLS or QUIC based on useTCP flag)
  4. TLS verification: TOFU (Trust-On-First-Use) with SHA-256 cert fingerprint pinning
  5. Sends JOIN handshake: "JOIN:<roomName>|<alias>|<password>\n"
  6. Creates ManagedConnection, stores in map
  7. Starts readLoop() goroutine for incoming messages
  Called by: Send() (line 379), main.go:226 (initial peer connections), main.go:449 (/connect)

connection_manager.go:285 — func (cm *ConnectionManager) readLoop(mc *ManagedConnection)
  Reads \n-delimited lines from outgoing connection.
  Handles: PONG (clears ping), PING (sends PONG), AUTH:FAILED (removes connection),
           SYSTEM:/FROM:/MSG:/plain text -> formats and prints to CLI.
  Uses IsDuplicate() to prevent echo loops (same message seen via multiple paths).
  On read error -> defer removeConnection()

connection_manager.go:360 — func (cm *ConnectionManager) removeConnection(peerAddr) bool
  Removes connection from map, closes it, cleans up pending ping. Returns true if found.
  Called by: readLoop defer, healthCheck, handleMessage AUTH:FAILED, server.removePeer

connection_manager.go:378 — func (cm *ConnectionManager) Send(ctx, peerAddr, message) error
  THE OUTGOING SEND. Calls getOrCreate() -> locks connection -> writes "FROM:<alias>|<msg>\n".
  Called by: main.go:420 (app.sendToRoom), main.go:457 (/connect with message)

connection_manager.go:411 — func (cm *ConnectionManager) RegisterIncoming(peerAddr, pc)
  Called by server.handlePeerConnection for incoming connections.
  Adds incoming connection to the same connections map (no handshake sent — peer initiated).
  NOTE: Does NOT start a readLoop — the server's handlePeerConnection handles reading.

connection_manager.go:74  — func (cm *ConnectionManager) IsDuplicate(sender, message) bool
  SHA-256 hash of sender+message, stored with timestamp. 60s TTL, cleaned every 30s.
  Prevents message echo when the same message arrives via multiple paths.

connection_manager.go:105 — func (cm *ConnectionManager) healthCheck()
  Every 5s: snapshots connections, sends PING to each, tracks pendingPings.
  If PONG not received within 10s -> marks as dead -> removeConnection().
  Lock ordering: RLock on connections -> Lock on pendingPings -> removeConnection (no locks held).
```

### main.go

```
main.go:69  — type App struct
  Fields: server, discovery, connManager, roomName, roomPassword, isPrivate, useTCP, rendezvousURL

main.go:273 — func initializeApp(roomName, isPrivate, useTCP, discPort, rendezvousURL, roomPassword) (*App, error)
  1. Creates ConnectionManager (localPort=0 initially)
  2. Creates Server with callbacks that print to CLI
  3. Sets connManager.localPort from server.Port()
  4. If not private -> creates DiscoveryService
  Called by: main.go:214

main.go:398 — func (app *App) sendToRoom(message string)
  THE LOCAL SEND PATH. Called when user types a message in CLI.
  1. Gets list of connected peers from ConnectionManager
  2. If no peers + discovery exists -> does a LookupPeers() broadcast
  3. Iterates all peer addresses -> calls connManager.Send() for each
  4. Uses 2s timeout per send batch
  Called by: runCLI (line 387) for non-command input
```

### transport.go

```
transport.go:10 — type PeerConnection interface
  Embeds: io.Reader, io.Writer, io.Closer, RemoteAddr() net.Addr
  This is the abstraction that unifies QUIC streams and TCP connections.

transport.go:17 — type QuicConnectionWrapper struct
  Fields: Stream (*quic.Stream), Conn (*quic.Conn)
  Read/Write delegate to Stream. Close closes both Stream and Conn.
  RemoteAddr delegates to Conn.RemoteAddr().

transport.go:43 — type NetConnWrapper struct
  Fields: Conn (net.Conn)
  Simple wrapper around net.Conn (used for TLS-over-TCP fallback).
```

### rendezvous.go

```
rendezvous.go:21 — func RenewRegistration(ctx, serverURL, roomName, addr) error
  POST /rooms/{roomName} with {"addr": "ip:port"} to rendezvous server.
  Called by: main.go:244 (every 30s in background goroutine)

rendezvous.go:47 — func FetchPeers(ctx, serverURL, roomName) ([]string, error)
  GET /rooms/{roomName} -> returns {"peers": ["ip:port", ...]}
  Called by: main.go:207 (at startup, before initializeApp)
```

---

## Data Flow: Peer A sends message to Peer B receiving it

### Current Full-Mesh Flow

```
User types "hello" at Peer A CLI
  -> app.runCLI() [main.go:334]
  -> app.sendToRoom("hello") [main.go:398]
    -> connManager.ListConnected() -> ["B:5000", "C:5001"]
    -> connManager.Send(ctx, "B:5000", "hello") [connection_manager.go:378]
      -> getOrCreate("B:5000") -> returns existing ManagedConnection
      -> writes "FROM:SwiftFox42|hello\n" to B's connection
    -> connManager.Send(ctx, "C:5001", "hello")
      -> same pattern

Peer B's connection_manager.readLoop() [connection_manager.go:285]
  -> reads "FROM:SwiftFox42|hello\n"
  -> IsDuplicate("SwiftFox42", "hello") -> false (first time seeing this)
  -> prints to CLI: "<SwiftFox42> hello"

Peer B's server also receives the message (if B is also connected to A via incoming):
  -> handlePeerConnection read loop [server.go:169]
  -> handleMessage(peer, "FROM:SwiftFox42|hello") [server.go:185]
  -> broadcastToRoom(room, B.addr, "hello") [server.go:236]
    -> sends "FROM:SwiftFox42|hello\n" to all peers EXCEPT B
  -> onMessage callback -> prints to CLI
  -> IsDuplicate check prevents double-print
```

### Key observation: TWO paths for the same message

1. **Outgoing path** (ConnectionManager.Send): Peer A actively sends to each known peer
2. **Incoming path** (Server.broadcastToRoom): When Peer A receives a message from someone, it re-broadcasts to its room peers

This creates a **gossip-like propagation** where messages can reach peers through multiple hops, but the deduplication (IsDuplicate) prevents infinite loops and double-display.

---

## Architecture for Relay Mode

### What needs to change

The current model assumes every peer connects to every other peer (full mesh). In relay mode:

1. **Room creator = relay node** — accepts connections from all peers
2. **Non-relay peers** — connect ONLY to the relay, not to each other
3. **Relay routes messages** — when relay receives a message from peer A, it forwards to all other peers in the room (this already happens via `broadcastToRoom`)

### Minimal changes needed

#### Option A: Flag-based relay mode (simplest)

Add a `--relay` flag. When enabled:
- The peer acts as a relay: it already does `broadcastToRoom()` for incoming messages
- Non-relay peers only connect to the relay (not to each other)
- The relay's `broadcastToRoom()` naturally routes messages between all connected peers

**What already works:** The server's `broadcastToRoom()` already forwards messages from any incoming peer to all other peers in the room. This IS relay behavior.

**What's missing:** Non-relay peers currently try to connect to ALL discovered peers (full mesh). They need to only connect to the relay.

#### Option B: Explicit relay topology

1. Add `isRelay` flag to Room struct
2. Room creator sets `isRelay = true`
3. Discovery messages include `IsRelay` field
4. Non-relay peers only connect to relay addresses
5. Relay peers don't forward messages to other relay peers (avoid loops)

### Key integration points

| Component | Current behavior | Relay behavior needed |
|-----------|-----------------|----------------------|
| `handlePeerConnection` | Registers incoming conn, reads messages, broadcasts to room | **Already works** — broadcasts to all room peers |
| `broadcastToRoom` | Sends to all peers except sender | May need to exclude relay-to-relay forwarding |
| `ConnectionManager.Send` | Sends to ALL connected peers | Non-relay should only send to relay |
| `sendToRoom` (main.go) | Iterates ALL connected peers | Non-relay should only send to relay |
| `getOrCreate` | Dials any peer address | Non-relay should only dial relay |
| Discovery | Broadcasts room + port | Needs to broadcast "I am a relay" flag |
| Rendezvous | Registers addr per room | May need relay designation |

### The critical insight

**The server's `broadcastToRoom()` is already a relay.** When a peer connects to the server and sends a message, the server forwards it to all other connected peers in the same room. The only thing preventing pure relay mode is that non-relay peers also try to connect directly to each other (full mesh), creating redundant paths.

To implement relay mode:
1. Add `--relay` flag to `main.go`
2. When `--relay` is set, the peer advertises itself as a relay in discovery broadcasts
3. Non-relay peers, when discovering peers, only connect to relay-flagged peers
4. The existing `broadcastToRoom()` handles all message routing

---

## Red Flags

- **Double message paths**: Messages can arrive via both `ConnectionManager.readLoop()` (outgoing connection) AND `Server.handlePeerConnection` (incoming connection). The `IsDuplicate()` dedup handles this, but it's wasteful.

- **No relay awareness in discovery**: `DiscoveryMessage` struct has no `IsRelay` field. All peers look the same.

- **ConnectionManager stores both incoming and outgoing**: `RegisterIncoming()` adds server-side connections to the same map as outgoing connections. `ListConnected()` returns both. In relay mode, this is actually useful — the relay can see all connected peers.

- **`sendToRoom` uses `ListConnected()`**: This returns ALL connections (both incoming and outgoing). For a relay, this is correct. For a non-relay in relay mode, it would try to send to everyone instead of just the relay.

- **No topology awareness**: The code has no concept of "I am a relay" vs "I am a leaf node". Every peer runs the same server + connection manager.

- **QUIC stream-per-connection**: Each QUIC connection uses exactly ONE stream (`AcceptStream` once). This means the protocol is effectively single-stream per connection. A relay handling many peers would need many QUIC connections (one per peer), which is fine but worth noting.

---

## What I couldn't find

- No existing relay/forwarding mode flag or configuration
- No concept of peer roles (relay vs leaf) in the protocol
- No message TTL or hop-count mechanism (relying solely on dedup for loop prevention)
- The rendezvous server implementation is external (not in this repo) — it's just a HTTP endpoint that stores room-to-peers mappings

---

## Follow-up questions

1. Should relay mode be opt-in (`--relay` flag on the creator) or should the first peer in a room automatically become the relay? The current code already gives the first peer implicit "creator" status (they set the room password).

2. Do you want pure star topology (all peers to relay only) or hybrid (relay + some direct connections for redundancy)? The current full-mesh is the extreme hybrid case.

3. Should the relay also participate in the conversation (as a normal peer) or be a pure forwarder? Currently `broadcastToRoom` excludes the sender, so a relay would see messages but not echo them back to the sender — but it would still display them locally via `onMessage`.
