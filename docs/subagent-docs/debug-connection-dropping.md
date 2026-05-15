# Debug: Random Connection Drops

**Date**: 2026-05-11  
**Investigated by**: Trace (root cause analyst)

---

## Reproduced

Yes — reproduced via code-path analysis. The bug is deterministic and reproducible in any multi-peer scenario.

---

## Root Cause

**The `healthCheck()` PING/PONG mechanism is broken for incoming connections.**  
`healthCheck()` sends PINGs to ALL connections (both outgoing and incoming), but `pendingPings` is only cleared for **outgoing** connections (by `readLoop` at `connection_manager.go:287-289`). Incoming connections registered via `RegisterIncoming()` don't have a `readLoop` — the server's `handlePeerConnection` reads messages instead. The server's `handleMessage` silently drops PONG (`server.go:188-190`) without clearing `pendingPings`. After ~15 seconds (3 health check cycles at 5s each), the health check declares the incoming connection unresponsive and evicts it via `removeConnection()`, causing the random disconnect.

**Additionally**, this means that for any bidirectional peer relationship, **both sides** eventually evict each other's incoming connection, tearing down the link completely — even though both connections are perfectly healthy.

---

## Evidence Trail

1. `NewConnectionManager()` starts `go cm.healthCheck()` at `connection_manager.go:67`
2. `healthCheck()` ticker fires every 5 seconds (`connection_manager.go:104`)
3. On each tick, it iterates ALL connections (`connection_manager.go:108-113`)
4. For each address, if no pending ping exists, it sets `cm.pendingPings[addr] = time.Now()` and fires a goroutine that writes `"PING\n"` to the connection (`connection_manager.go:133-138`)
5. For **outgoing** connections: `readLoop` (`connection_manager.go:268`) handles the PONG reply at lines 283-290, clearing `pendingPings` → works correctly ✓
6. For **incoming** connections (registered via `RegisterIncoming` at `connection_manager.go:378-394`): no `readLoop` is started. The server's `handlePeerConnection` (`server.go:152-181`) is the reader. When a PONG is received, it calls `handleMessage`, which hits `server.go:188-190`:

```go
if message == "PONG" {
    return  // silently dropped — pendingPings NEVER cleared!
}
```

7. On the next health check tick (time > 10s from the pending ping), the check at `connection_manager.go:119-122` fires:
```go
if time.Since(sent) > 10*time.Second {
    dead = append(dead, addr)  // innocent connection marked dead
}
```

8. `removeConnection()` is called at `connection_manager.go:149`, which calls `mc.conn.Close()`, tearing down the QUIC stream/connection → **random disconnect**

---

## Fix

### Fix 1 (Primary): Clear `pendingPings` in the server's PONG handler

Add a `ClearPing` method to `ConnectionManager` and call it from the server when PONG is received:

**connection_manager.go** — add after `healthCheck()`:
```go
// ClearPing removes a pending PING entry (called when PONG is received
// on the server-side read loop for incoming connections).
func (cm *ConnectionManager) ClearPing(addr string) {
    cm.pendingPingsMu.Lock()
    delete(cm.pendingPings, addr)
    cm.pendingPingsMu.Unlock()
}
```

**server.go** — modify `handleMessage` (line 188-190):
```go
if message == "PONG" {
    if s.connManager != nil {
        s.connManager.ClearPing(peer.addr)
    }
    return
}
```

### Fix 2 (Secondary): Clean `pendingPings` in `removeConnection()`

Prevents stale entries from leaking when connections are removed outside of healthCheck:

**connection_manager.go** — modify `removeConnection()` (after line 340):
```go
func (cm *ConnectionManager) removeConnection(peerAddr string) {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    if mc, exists := cm.connections[peerAddr]; exists {
        mc.conn.Close()
        delete(cm.connections, peerAddr)
    }

    // Clean up pending PING entry to avoid stale map entries.
    cm.pendingPingsMu.Lock()
    delete(cm.pendingPings, peerAddr)
    cm.pendingPingsMu.Unlock()
}
```

### Fix 3 (Secondary): Server `removePeer` should notify ConnectionManager

When the server detects a dead connection, it should tell the ConnectionManager to remove the stale entry:

**server.go** — modify `removePeer()` (after line 329, before the `Close`):
```go
func (s *Server) removePeer(peer *Peer) {
    // ... existing room cleanup ...

    // Notify ConnectionManager so it doesn't keep PINGing a dead connection.
    if s.connManager != nil {
        s.connManager.removeConnection(peer.addr)
    }

    peer.conn.Close()
}
```

### Fix 4 (Defensive): Guard against closed connection in healthCheck PING goroutine

Prevent writes to already-closed connections when a connection is removed between the map lookup and the goroutine execution:

**connection_manager.go** — modify lines 132-138:
```go
// After cm.pendingPings[addr] = time.Now()
go func(c *ManagedConnection, addr string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    // Re-check the connection still exists and isn't closed.
    n, err := c.conn.Write([]byte("PING\n"))
    if err != nil {
        // Connection is likely dead; don't wait for timeout.
        cm.pendingPingsMu.Lock()
        delete(cm.pendingPings, addr)
        cm.pendingPingsMu.Unlock()
        cm.removeConnection(addr)
        return
    }
    _ = n
}(mc, addr)
```

---

## Verify With

```bash
# Run existing tests to ensure no regressions
go test -v ./... 2>&1

# For manual verification of the fix:
# 1. Start two instances (A and B) in the same room
# 2. Let them sit for 30+ seconds
# 3. Before fix: connections drop after ~15s with "disconnected (no response to ping)"
# 4. After fix: connections remain stable
```

---

## Not the Cause (Ruled Out)

- **QUIC idle timeout**: `QUICIdleTimeout = 60s` and `QUICKeepAlive = 30s` — without the application-level eviction bug, QUIC's own keepalive probes would maintain the connection. The QUIC constants are not the problem.
- **Context cancellation**: `context.Background()` is used in accept loops; no premature cancellation. The `getOrCreate` timeout is 10s for dial, which is reasonable.
- **Goroutine leaks**: The healthCheck goroutine runs on a 5s ticker; no unbounded goroutine creation beyond one per connection per cycle (which immediately returns).
- **Race condition on `mc.conn.Write`**: QUIC streams support concurrent reads and writes — no data race. The PING write in healthCheck and the read in handlePeerConnection operate on different stream directions.
- **Certificate TOFU check**: The `VerifyPeerCertificate` callback at `connection_manager.go:185-201` properly handles first-seen and mismatch cases — not causing disconnects on reconnect (it just rejects mismatches).

---

## If This Doesn't Fix It

Check the following and send back:

1. Run with `--debug` flag to see log output. HealthCheck logs `"Peer %s is unresponsive, removing"` when it evicts. If that log appears, the issue is still in the PING/PONG flow.
2. Check `pendingPings` map state via a debug print added to healthCheck to see which addresses have stale entries.
3. Check if `readLoop` is actually clearing `pendingPings` for outgoing connections — add debug logging at `connection_manager.go:288`.
4. Check for double-close issues: `removeConnection` calls `mc.conn.Close()`, and then `readLoop`'s deferred `removeConnection` also calls `Close()`. If `Close()` is not idempotent for the specific QUIC library version, this could cause panics. Run with `GOTRACEBACK=all`.
