# Debug: PING/PONG Round 2 — Why Peers Still Disconnect

## Reproduced

Not reproduced in unit tests (all pass). The issue is observed at runtime — peers disconnect with "no response to ping" after ~10 seconds. Static code-path analysis was performed on the complete PING/PONG flow.

---

## Root cause

**The PING/PONG handling in `readLoop()` and `handleMessage()` is correct for both incoming and outgoing connections.** However, `healthCheck()` in `connection_manager.go` has a **TOCTOU race condition** (lines 115-119 and 157) where a stale snapshot of `cm.connections` can cause a newly-created connection to be incorrectly removed when a reconnection happens during a health-check tick. Additionally, `broadcastToRoom()` in `server.go` (line 328) writes to `peer.conn` without synchronizing with `healthCheck()`'s writes to the same underlying stream via `mc.conn`, which could corrupt the stream on incoming connections when a broadcast coincides with a PING.

---

## Evidence trail

### 1. Does `readLoop()` handle PING? YES — lines 334-339 of `connection_manager.go`
```go
if line == "PING" {
    mc.mu.Lock()
    mc.conn.Write([]byte("PONG\n"))
    mc.mu.Unlock()
    continue
}
```
It replies to any received "PING" with "PONG\n" on the same stream.

### 2. Does `readLoop()` clear pending pings on PONG? YES — lines 326-331
```go
if line == "PONG" {
    mc.mu.Lock()
    mc.lastUsed = time.Now()
    mc.mu.Unlock()
    cm.ClearPing(mc.peerAddr)
    continue
}
```

### 3. Does `handleMessage()` in server.go handle PING? YES — lines 186-189
```go
if message == "PING" {
    peer.conn.Write([]byte("PONG\n"))
    return
}
```

### 4. Does `handleMessage()` clear pending pings on PONG? YES — lines 190-194
```go
if message == "PONG" {
    if s.connManager != nil {
        s.connManager.ClearPing(peer.addr)
    }
    return
}
```
This was added in commit `0d0fc1e` ("fix asymmetric PING/PONG keepalive").

### 5. Full PING/PONG flow tracing — both directions work

**Direction 1: Peer A sends PING to Peer B**
- A's `healthCheck()` writes "PING\n" on A's connection to B
- B receives it:
  - If outgoing (B dialed A): B's `readLoop()` handles PING → replies PONG
  - If incoming (A dialed B): B's `handleMessage()` handles PING → replies PONG
- A receives PONG:
  - If outgoing (A dialed B): A's `readLoop()` → `ClearPing()`
  - If incoming (B dialed A): A's `handleMessage()` → `ClearPing()`

**Direction 2: Peer B sends PING to Peer A**
- Same flow, symmetric. Both sides can send and receive PING/PONG regardless of who dialed whom.

### 6. But: NO `readLoop` for incoming connections

Incoming connections are registered via `RegisterIncoming()` (line 437-453), which does **NOT** start a `readLoop` goroutine. The only reader for incoming connections is `handlePeerConnection()` in server.go (line 154-183). This is by design:
- Outgoing connections → `getOrCreate()` starts `readLoop`
- Incoming connections → `handlePeerConnection()` reads

This means: if `handlePeerConnection()`'s goroutine exits for any reason **without** calling `removeConnection()` (e.g., a panic), the connection stays in `cm.connections` but nobody reads PONG responses. The pending ping mechanism times out, and `healthCheck()` eventually calls `removeConnection()` to clean up. But between the exit and the timeout (~10s), the connection is zombie.

There is **no `recover()`** anywhere in the accept/read call chain: `acceptLoopQUIC` → `handleQUICConnection` → `handlePeerConnection`. A panic in `handleMessage()` or any called function would kill the goroutine, leaving a zombie connection in `cm.connections`.

### 7. TOCTOU race condition in `healthCheck()` (lines 115-157)

```go
// Step 1: Snapshot under RLock
cm.mu.RLock()
conns := make(map[string]*ManagedConnection, len(cm.connections))
for addr, mc := range cm.connections {
    conns[addr] = mc
}
cm.mu.RUnlock()

// ... later ...

// Step 2: Write PING to stale ManagedConnection from snapshot
c.mu.Lock()
_, err := c.conn.Write([]byte("PING\n"))
c.mu.Unlock()
if err != nil {
    // Step 3: Write fails → calls removeConnection(addr)
    // This deletes WHATEVER is at cm.connections[addr] — possibly a NEW, valid connection
    cm.removeConnection(addr)
}
```

**Race sequence:**
1. `healthCheck` snapshots: conns = {"X": mc_old}
2. `readLoop` for mc_old exits → `removeConnection("X")` → cm.connections = {}, mc_old.conn.Close()
3. `getOrCreate("X")` creates mc_new → cm.connections = {"X": mc_new}
4. `healthCheck` writes PING to mc_old (closed) → fails → calls `removeConnection("X")`
5. `removeConnection("X")` deletes mc_new from cm.connections and closes mc_new.conn

**Result**: A freshly-established, healthy connection is destroyed because `healthCheck` was working with a stale reference.

### 8. Concurrent write to QUIC stream without synchronization

`broadcastToRoom()` (server.go line 328) writes to `p.conn` (raw PeerConnection) without acquiring `mc.mu`. Meanwhile, `healthCheck()` writes to `mc.conn` (same underlying stream) under `mc.mu.Lock()`. For incoming connections, these are the SAME QUIC stream. Two goroutines writing concurrently to a QUIC stream can corrupt data — the quic-go docs state: "Writes are not safe for concurrent use."

This could garble PING/PONG messages mid-flight, causing them to not match the expected "PING" or "PONG" string after `TrimSpace`.

---

## Fixes

### Fix 1: Health-check should NOT use stale ManagedConnection references

Instead of writing through a stale pointer, re-fetch from the map:

```go
// connection_manager.go, healthCheck(), around line 148
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
```

Key change: re-lookup `cm.connections[addr]` before writing, so we never Write to a closed/replaced connection.

### Fix 2: Protect broadcast writes with the ManagedConnection mutex (or use a stream-level write lock)

In `handlePeerConnection`, the peer's connection is the same object as `mc.conn` for that address. Either:
- (a) Have `broadcastToRoom` acquire `mc.mu` before writing, OR
- (b) Use a separate write-lock on the underlying stream

Simplest approach: modify `broadcastToRoom` to check `cm.connections` for the peer's ManagedConnection and use its mutex:

```go
// server.go broadcastToRoom, before p.conn.Write
if s.connManager != nil {
    // Lock the ManagedConnection mutex for safe concurrent write
    // (avoids stream corruption with healthCheck PING writes)
    // This requires adding a helper: cm.LockConn(addr) / cm.UnlockConn(addr)
}
```

But a simpler fix is to make `healthCheck` use a global or connection-level write mutex, or to make `broadcastToRoom` route writes through `ConnectionManager.Send()` which already serializes writes.

### Fix 3 (defense-in-depth): Add panic recovery to handlePeerConnection

```go
// server.go, handlePeerConnection, at the top
defer func() {
    if r := recover(); r != nil {
        log.Printf("[Server] panic in handlePeerConnection for %s: %v", peerAddr, r)
        s.removePeer(peer)
    }
}()
```

This ensures that even if `handleMessage` or any callee panics, the connection is properly removed from `cm.connections`.

---

## Verify with

```bash
# Run existing tests to confirm no regression
go test -v -run TestHandleMessagePing ./...
go test -v -run TestHandleMessagePong ./...
go test -v -run TestRelayStarTopology ./... 2>&1

# For manual verification: run two peers, wait 15+ seconds, check no disconnect
# Expected: no "disconnected (no response to ping)" messages
```

---

## Not the cause (ruled out)

- **PING/PONG asymmetry in handleMessage**: Fixed in commit `0d0fc1e` — `handleMessage` now calls `ClearPing` on PONG.
- **readLoop not handling PING**: It does (lines 334-339).
- **Address mismatch between RegisterIncoming and ClearPing**: Both use the same `peerAddr` from `conn.RemoteAddr().String()`.
- **Double-read from same stream**: Only one goroutine reads each connection — `readLoop` for outgoing, `handlePeerConnection` for incoming.
- **Missing lastUsed update on PING in readLoop**: readLoop doesn't update `lastUsed` when it receives PING. This is minor — `lastUsed` is only checked in test code, not in production health logic.

---

## If this doesn't fix it

1. Add debug logging to `healthCheck()` to trace exact addresses in `pendingPings`, `dead`, and `needPing` lists.
2. Add logging to `ClearPing()` to confirm it's being called with the expected address.
3. Check if the QUIC stream's concurrent-write corruption is the issue: log the raw bytes read by `readLoop`/`handlePeerConnection` before `TrimSpace`.
4. Test with `--use-tcp` vs QUIC to isolate whether the issue is transport-specific.
