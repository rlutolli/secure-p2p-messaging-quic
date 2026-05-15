# Chat Room Password Support — Investigation Findings

## Summary

**There is currently NO password protection for chat rooms.** Rooms are created implicitly on first join, are entirely open, and any peer that knows the room name (or discovers it via UDP broadcast) can join without any authentication. A `DeriveRoomKey(roomName, password)` stub exists in `security.go` but is a no-op — it returns an empty `RoomCrypto{}` and is only referenced by a single test.

---

## 1. Data Structures — Where Rooms Are Defined

### `Room` struct — `server.go:41-45`
```go
type Room struct {
    name    string
    peers   map[string]*Peer
    peersMu sync.RWMutex
}
```
- **No password field.** The struct only holds a name and a peer map.
- Rooms are stored in-memory in `Server.rooms` (`map[string]*Room`).
- No persistence — rooms vanish when the last peer leaves (see `removePeer`, `server.go:314-317`).

### `Peer` struct — `server.go:47-52`
```go
type Peer struct {
    addr  string
    alias string
    conn  PeerConnection
    room  *Room
}
```
- No password or auth state tracked per peer.

### `Server` struct — `server.go:30-39`
```go
type Server struct {
    quicListener    *quic.Listener
    tcpListener     net.Listener
    rooms           map[string]*Room
    roomsMu         sync.RWMutex
    localPort       int
    onMessage       func(from, room, message string)
    onSystemMessage func(message string)
    connManager     *ConnectionManager
}
```
- No global password policy or room registry with auth metadata.

### `RoomCrypto` struct — `security.go:104-106`
```go
type RoomCrypto struct {
    roomKey []byte
}
```
- **Exists but unused.** The `roomKey` field is never populated.
- `DeriveRoomKey` at `security.go:108-109` is a **stub** — returns `&RoomCrypto{}` with nil `roomKey`.

### `DiscoveryMessage` — `discovery.go:21-26`
```go
type DiscoveryMessage struct {
    Type    string `json:"type"`
    Room    string `json:"room"`
    Port    int    `json:"port"`
    Version string `json:"version"`
}
```
- No password or auth field. Room names are broadcast in plaintext during discovery.

### `App` struct — `main.go:69-77`
```go
type App struct {
    server        *Server
    discovery     *DiscoveryService
    connManager   *ConnectionManager
    roomName      string
    isPrivate     bool
    useTCP        bool
    rendezvousURL string
}
```
- Has `isPrivate` flag (hides from discovery) but **no password field**.

---

## 2. Where Rooms Are Created

**Rooms are created implicitly — there is no explicit CREATE command.**

### `server.go:236-261` — `joinRoom()`
```go
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
    // ... add peer, broadcast join message
}
```
- **First join creates the room.** No password check, no creator ownership.
- Any peer sending `JOIN:<roomName>` either joins an existing room or creates a new one.

### `main.go:159-165` — CLI room creation
```go
case "n", "N":
    fmt.Print("Enter new room name: ")
    roomName, _ = reader.ReadString('\n')
    roomName = strings.TrimSpace(roomName)
    if roomName == "" {
        roomName = "default"
    }
```
- User picks a name — no password prompt.
- The room name is passed to `NewConnectionManager(roomName, ...)` which uses it in the handshake.

---

## 3. Where Peers Join Rooms

### Protocol-level join — `connection_manager.go:248`
```go
handshake := fmt.Sprintf("JOIN:%s|%s\n", cm.roomName, cm.localAlias)
```
- The **only** join message format is `JOIN:<roomName>|<alias>`.
- **No password slot in the protocol.**

### Server-side join handler — `server.go:192-200`
```go
if strings.HasPrefix(message, "JOIN:") {
    payload := strings.TrimPrefix(message, "JOIN:")
    parts := strings.SplitN(payload, "|", 2)
    roomName := parts[0]
    if len(parts) == 2 && parts[1] != "" {
        peer.alias = parts[1]
    }
    s.joinRoom(peer, roomName)
    return
}
```
- Parses `JOIN:<roomName>|<alias>` — only two fields.
- Calls `joinRoom()` directly with no auth check.

---

## 4. Protocol Message Format

### Current protocol (documented in `server.go:4-12`):

| Direction | Format | Description |
|-----------|--------|-------------|
| Incoming | `JOIN:<roomName>|<alias>` | Join a room |
| Incoming | `FROM:<alias>|<message>` | User message |
| Incoming | `MSG:<message>` | Legacy message |
| Incoming | `<plain text>` | Raw message (if in room) |
| Incoming | `PING` / `PONG` | Keepalive |
| Outgoing | `SYSTEM:<message>` | System notifications |
| Outgoing | `FROM:<alias>|<message>` | Broadcast to peers |

**No AUTH, no CHALLENGE, no PASSWORD message types exist.**

---

## 5. Existing Authentication / Authorization Logic

### What exists:
1. **TLS 1.3** — `security.go:36-88` — All connections are encrypted via self-signed TLS certificates with dynamic SANs for local IPs. This provides transport-level encryption but **no room-level access control**.

2. **TOFU (Trust-On-First-Use) pinning** — `connection_manager.go:185-201` — Stores SHA-256 fingerprints of peer certificates. Rejects reconnections with different certs. This prevents MITM but **does not restrict room access**.

3. **`isPrivate` flag** — `main.go:167-169` — When room name is "private", the discovery service is not started. The room won't appear in UDP broadcast scans. But **anyone who knows the address and room name can still join** — it's "security by obscurity" only.

4. **`DeriveRoomKey` stub** — `security.go:108-109` — Takes `(roomName, password)` but returns an empty struct. Tested in `security_test.go:26-35` but never called from production code. This appears to be **planned infrastructure that was never implemented**.

### What does NOT exist:
- No password verification on join
- No room creator / owner concept
- No challenge-response handshake
- No message-level encryption per room (beyond TLS)
- No access control lists

---

## 6. Password Field Search Results

The only password-related code in the entire codebase:

| File | Line | Content |
|------|------|---------|
| `security.go` | 108 | `func DeriveRoomKey(roomName, password string) (*RoomCrypto, error)` — stub, returns empty struct |
| `security_test.go` | 28 | `DeriveRoomKey("testroom", "password")` — only test usage |

**No password field exists on `Room`, `Peer`, `Server`, `App`, `DiscoveryMessage`, or any other struct.**

---

## 7. Room State Management

- **In-memory only.** `Server.rooms` is a `map[string]*Room`.
- **Auto-deletion:** When the last peer leaves a room, the room is deleted from the map (`server.go:314-317`).
- **No persistence across restarts.**
- **No room metadata** beyond name and peer list.
- Thread-safe via `sync.RWMutex` on both `Server.roomsMu` and `Room.peersMu`.

---

## 8. What Needs to Change to Add Password-Protected Rooms

### A. Data Structure Changes

**`server.go` — `Room` struct (line 41):**
```go
type Room struct {
    name         string
    passwordHash string  // NEW: empty = no password, set = password required
    creator      string  // NEW (optional): who created the room
    peers        map[string]*Peer
    peersMu      sync.RWMutex
}
```

**`main.go` — `App` struct (line 69):**
```go
type App struct {
    // ... existing fields ...
    roomPassword string  // NEW: password for this room
}
```

**`connection_manager.go` — `ConnectionManager` struct (line 23):**
```go
type ConnectionManager struct {
    // ... existing fields ...
    roomPassword string  // NEW: password to send during handshake
}
```

### B. Protocol Changes

**New JOIN format** (backward-compatible extension):
```
JOIN:<roomName>|<alias>|<password>
```
- Third field is optional. If omitted, treated as empty string (no password).
- Existing peers sending `JOIN:<roomName>|<alias>` would still work for non-password rooms.

**New server response for auth failure:**
```
AUTH:FAILED|<reason>
```
- Sent when password doesn't match. Connection should be closed after.

**New server response for auth success (optional):**
```
AUTH:OK
```
- Could be sent before the join broadcast for explicit confirmation.

### C. Server Logic Changes

**`server.go` — `handleMessage()` (around line 192):**
- Parse the optional third field from JOIN message.
- Before calling `joinRoom()`, check if room has a password set.
- If room exists with password: verify the provided password against stored hash.
- If room doesn't exist and password is provided: create room with that password hash.
- If room doesn't exist and no password: create room without password (current behavior).
- On mismatch: send `AUTH:FAILED` and close connection.

**`server.go` — `joinRoom()` (line 236):**
- Add optional `passwordHash` parameter.
- When creating a new room, store the password hash if provided.

### D. Discovery Protocol Changes

**`discovery.go` — `DiscoveryMessage` (line 21):**
```go
type DiscoveryMessage struct {
    Type         string `json:"type"`
    Room         string `json:"room"`
    Port         int    `json:"port"`
    Version      string `json:"version"`
    HasPassword  bool   `json:"has_password,omitempty"`  // NEW
}
```
- Allows room browser to show a lock icon for password-protected rooms.
- Does NOT broadcast the password itself.

### E. CLI Changes

**`main.go` — room creation flow (around line 159):**
- After entering room name, prompt: "Set a password? (leave empty for none): "
- Store password in `App.roomPassword`.
- Pass to `NewConnectionManager()` and discovery service.

**`main.go` — room selection from discovered list (around line 146):**
- If `HasPassword` is true, prompt user for password before connecting.

### F. Connection Manager Changes

**`connection_manager.go` — `getOrCreate()` (line 248):**
```go
handshake := fmt.Sprintf("JOIN:%s|%s|%s\n", cm.roomName, cm.localAlias, cm.roomPassword)
```
- Include password in JOIN handshake.

**`connection_manager.go` — `readLoop()` (line 268):**
- Handle `AUTH:FAILED` response: disconnect and show error to user.

### G. Implement `DeriveRoomKey` Properly

**`security.go` — `DeriveRoomKey()` (line 108):**
```go
func DeriveRoomKey(roomName, password string) (*RoomCrypto, error) {
    // Use argon2id or scrypt to derive a key from password + roomName salt
    // Store the hash for comparison during join
}
```
- Should use a proper KDF (e.g., `golang.org/x/crypto/argon2` or `scrypt`).
- The derived key can serve as both the stored hash and optionally as a room-level encryption key for message payloads.

### H. Rendezvous Server Consideration

**`rendezvous.go`** — The HTTP rendezvous server (external) registers peers by room name at `/rooms/<roomName>`. If password protection is added:
- The rendezvous server should NOT store or transmit passwords.
- It may need a `has_password` flag in the room metadata so clients know to prompt.

---

## 9. Files That Need Modification

| File | Changes Needed |
|------|---------------|
| `server.go` | Add `passwordHash` to `Room`; modify `handleMessage()` to parse/verify password; modify `joinRoom()` to set password on creation; add `AUTH:FAILED` response |
| `connection_manager.go` | Add `roomPassword` field; include password in JOIN handshake; handle `AUTH:FAILED` in readLoop |
| `main.go` | Add password prompt during room creation/join; pass password to ConnectionManager; update CLI help |
| `discovery.go` | Add `HasPassword` field to `DiscoveryMessage`; broadcast it in announces |
| `security.go` | Implement `DeriveRoomKey()` with proper KDF (argon2id/scrypt) |
| `rendezvous.go` | Optionally add `has_password` to room registration (if rendezvous server supports it) |
| `server_test.go` | Add tests for password-protected join, wrong password rejection, room creation with password |
| `security_test.go` | Add tests for `DeriveRoomKey()` producing consistent hashes, different passwords producing different keys |

---

## 10. Backward Compatibility Considerations

- The `|` delimiter is already used in the JOIN format. Adding a third field is backward-compatible if the server handles missing third field as "no password."
- Old clients sending `JOIN:room|alias` will be treated as sending an empty password. They can join non-password rooms but will be rejected from password-protected rooms.
- The discovery `HasPassword` field is optional (omitempty) — old clients will simply ignore it.

---

## Red Flags

1. **`DeriveRoomKey` is a stub** — It was clearly intended for room-level encryption but was never implemented. The test only checks that it returns non-nil, not that it actually derives anything.

2. **No rate limiting on joins** — An attacker could brute-force room passwords by repeatedly sending JOIN messages with different passwords.

3. **Password transmitted in plaintext** — Even with TLS, the password travels over the wire in the JOIN message. If a room password is meant to be a shared secret, this is acceptable within TLS. But if the threat model includes compromised peers, the password should be hashed client-side before transmission (or use a challenge-response protocol).

4. **Room passwords stored in plaintext** — If we add a `passwordHash` field, we must ensure it is a proper hash (not the raw password). The stub `DeriveRoomKey` suggests this was the intent.

5. **Discovery broadcasts room names openly** — Even password-protected rooms will have their names visible in UDP discovery broadcasts. The `HasPassword` flag would actually make them more visible as targets.

---

## What I Could Not Find

- There is no external rendezvous server code in this repo — it is referenced as an HTTP endpoint (`rendezvous.go`) but the server implementation is not included. The password feature would need coordination with that server if it should advertise `has_password`.
- No configuration file or environment variable support — all settings are CLI flags or hardcoded.
- No existing message-level encryption beyond TLS transport encryption. The `RoomCrypto` struct suggests this was planned but never built out.
