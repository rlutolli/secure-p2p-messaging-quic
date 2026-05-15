# WAN / Internet Connectivity — Investigation Findings

## Summary

The application is **primarily LAN-focused** with a **partial WAN scaffolding** already in place. UDP broadcast discovery works only on the local network. An HTTP rendezvous client exists for WAN peer discovery, but the rendezvous **server is external and not included in this repo**. There is **zero NAT traversal logic** — no STUN for connection setup, no TURN relay, no UPnP port forwarding, and no hole-punching. TLS certificates are generated with local-interface SANs only, which will cause certificate validation issues when connecting via public IPs. Two peers behind different NATs **cannot currently connect to each other** without manual port forwarding.

---

## 1. Discovery — How It Currently Works

### UDP Broadcast (LAN only) — `discovery.go`

**Mechanism:** JSON-based UDP broadcast on a configurable port (default `19999`).

**Broadcast targets** (`discovery.go:15-19`):
```go
var broadcastAddresses = []string{
    "255.255.255.255",   // Limited broadcast
    "127.255.255.255",   // Loopback broadcast
    "127.0.0.1",         // Direct loopback
}
```

**DiscoveryMessage format** (`discovery.go:21-26`):
```go
type DiscoveryMessage struct {
    Type    string `json:"type"`     // "announce" or "query"
    Room    string `json:"room"`
    Port    int    `json:"port"`     // The QUIC/TCP listening port
    Version string `json:"version"`  // "0.4"
}
```

**Flow:**
1. `announce()` broadcasts every 3 seconds (`discovery.go:165-178`)
2. `listenLoop()` receives announcements and stores peers with a 15-second expiry (`discovery.go:212-222`)
3. `LookupPeers()` sends a query, waits 500ms, returns collected peers (`discovery.go:224-241`)

**WAN limitation:** UDP broadcasts **do not cross NAT boundaries or routers**. `255.255.255.255` is strictly local-subnet. This mechanism is **completely useless for WAN**.

### HTTP Rendezvous Client (WAN-ready) — `rendezvous.go`

**Two functions exist:**

1. **`RenewRegistration(ctx, serverURL, roomName, addr)`** — `rendezvous.go:21-45`
   - `POST /rooms/<roomName>` with JSON body `{"addr": "<ip:port>"}`
   - Called every 30 seconds in a goroutine (`main.go:227-236`)
   - Registers the peer's address with the rendezvous server

2. **`FetchPeers(ctx, serverURL, roomName)`** — `rendezvous.go:47-75`
   - `GET /rooms/<roomName>`
   - Returns `{"peers": ["ip:port", ...]}`
   - Called once at startup before connecting (`main.go:191-198`)

**Critical gap:** The rendezvous **server implementation is NOT in this repo**. It's an external HTTP service that must be deployed separately. The client code is well-structured and tested (`rendezvous_test.go`), but without a running server, `--rendezvous <url>` does nothing.

### Discovery Activation in main.go

- `--rendezvous <url>` flag enables rendezvous (`main.go:108-112`)
- At startup, LAN discovery runs first (4-second scan), then rendezvous peers are fetched and merged (`main.go:191-198`)
- If rendezvous URL is provided and room is not private, the app registers itself every 30 seconds (`main.go:222-237`)

---

## 2. TLS Certificate Generation — SANs Limited to Local Interfaces

### `security.go:36-88` — `generateTLSConfig()`

**Certificate properties:**
- Self-signed (template is its own issuer — `x509.CreateCertificate(rand.Reader, &template, &template, ...)`)
- ECDSA P-256 key
- 7-day validity (`NotAfter: time.Now().Add(7 * 24 * time.Hour)`)
- TLS 1.3 minimum
- ALPN: `p2p-messenger/1.0`

**SANs** (`security.go:90-102` — `collectLocalIPs()`):
```go
func collectLocalIPs() []net.IP {
    ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
    ifaces, _ := net.Interfaces()
    for _, iface := range ifaces {
        addrs, _ := iface.Addrs()
        for _, addr := range addrs {
            if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP != nil {
                ips = append(ips, ipnet.IP)
            }
        }
    }
    return ips
}
```

**WAN problem:** The certificate only includes **local interface IPs** (127.0.0.1, ::1, and whatever `net.Interfaces()` returns — typically `192.168.x.x`, `10.x.x.x`, etc.). It does **NOT** include:
- The public/WAN IP address
- Any hostname or domain name
- Any dynamically discovered external IP

**Impact:** When Peer A (behind NAT) connects to Peer B's public IP, the TLS handshake will succeed because `InsecureSkipVerify: true` is set in `connection_manager.go:181`. However, the TOFU fingerprint pinning (`connection_manager.go:185-201`) will work fine since it's hash-based, not name-based. **The SAN limitation is not a blocker** because the client skips hostname verification, but it's a security concern — there's no way to validate the peer's identity beyond the first-seen fingerprint.

---

## 3. NAT Traversal — What Exists and What's Missing

### What EXISTS:

| Technique | Status | Details |
|-----------|--------|---------|
| **STUN (for IP discovery)** | Partial | `resolvePublicIP()` in `main.go:475-502` uses `stun.l.google.com:19302` to discover the public IP. Used only for display (`/myip` command) and for the rendezvous registration address. |
| **Rendezvous client** | Present | HTTP-based peer address exchange via `rendezvous.go`. |

### What's MISSING:

| Technique | Status | Impact |
|-----------|--------|--------|
| **STUN (for NAT binding)** | None | No STUN binding requests to create NAT mappings before connecting. |
| **TURN relay** | None | No fallback relay server when direct connection fails. |
| **UPnP / NAT-PMP** | None | No automatic port forwarding on the router. |
| **UDP hole-punching** | None | No coordinated simultaneous connection attempts. |
| **ICE (Interactive Connectivity Establishment)** | None | No candidate gathering or connectivity checks. |

### Current connection flow (`connection_manager.go:162-266`):

```
GetOrCreate(peerAddr "ip:port")
  -> quic.DialAddr(ctx, peerAddr, ...)  OR  net.Dialer.DialContext(ctx, "tcp", peerAddr)
  -> TLS handshake (InsecureSkipVerify: true)
  -> TOFU fingerprint check
  -> Send JOIN handshake
```

**This is a direct connection only.** If `peerAddr` is a private IP (e.g., `192.168.1.50:12345`) and the connecting peer is on a different network, the connection will **fail immediately** — the IP is not routable.

---

## 4. Rendezvous Server — How It Works

### Client-side protocol (from `rendezvous.go` + `rendezvous_test.go`):

**Registration:**
```
POST /rooms/<roomName>
Content-Type: application/json
{"addr": "<public-ip>:<port>"}

-> 200 OK (success)
-> 4xx/5xx (failure)
```

**Peer lookup:**
```
GET /rooms/<roomName>

-> 200 OK {"peers": ["1.2.3.4:5000", "5.6.7.8:6000"]}
-> 4xx/5xx (failure)
```

### Server-side: NOT IN THIS REPO

The rendezvous server is an external HTTP service. The client code assumes:
- It accepts POST to register a peer address
- It returns a list of peer addresses on GET
- It does NOT require authentication
- It does NOT validate the addresses

**The address registered is determined by `resolvePublicIP()` (`main.go:475-502`):**
```go
publicAddr := fmt.Sprintf("?:%d", app.server.Port())
if ip := resolvePublicIP(); ip != "" {
    publicAddr = fmt.Sprintf("%s:%d", ip, app.server.Port())
}
```

If STUN fails (e.g., behind symmetric NAT, firewall blocks UDP), the registered address is `?:<port>` — which is **useless** for other peers trying to connect.

### What the rendezvous server would need to be:
- A simple HTTP server (Go, Node.js, Python — anything)
- In-memory or Redis-backed room-to-peers map
- TTL-based peer expiry (peers register every 30s; server should evict stale entries)
- Optionally: authentication, rate limiting, TLS

---

## 5. What Would Be Needed for Two NAT'd Peers to Connect

### Scenario: Peer A (home NAT) <-> Peer B (office NAT)

**Current state: IMPOSSIBLE without manual intervention.**

Here's what's needed, in order of complexity:

### Option A: Manual Port Forwarding (simplest, but user-hostile)
1. Peer A configures their router to forward UDP port X to their machine
2. Peer A discovers their public IP (already works via `/myip`)
3. Peer A shares `public-ip:X` with Peer B out-of-band
4. Peer B uses `/connect public-ip:X`
5. **Works** — but requires router admin access and manual config

### Option B: UPnP / NAT-PMP (automatic port forwarding)
1. Add UPnP client library (e.g., `github.com/huin/goupnp`)
2. On startup, request the router to forward the QUIC/TCP port
3. Register the public IP + forwarded port with the rendezvous server
4. **Works for most home routers** — fails on enterprise NATs and some ISPs

### Option C: UDP Hole-Punching (requires rendezvous server coordination)
1. Both peers register with the rendezvous server
2. Server tells each peer the other's **NAT-mapped** address (IP:port as seen by the server)
3. Both peers simultaneously send UDP packets to each other's mapped addresses
4. This "punches" holes in both NATs
5. QUIC connections can then be established
6. **Works for cone NATs** — fails for symmetric NATs

### Option D: TURN Relay (fallback, guaranteed to work)
1. Deploy a TURN server (e.g., `coturn`)
2. When direct connection fails, both peers connect to the TURN server
3. TURN server relays traffic between them
4. **Always works** — but adds latency and requires server infrastructure

### Option E: Full ICE Implementation (the WebRTC approach)
1. Gather candidates: host, STUN-derived (srflx), and TURN (relay)
2. Exchange candidates via signaling server (the rendezvous server)
3. Perform connectivity checks
4. Select the best working candidate pair
5. **Most robust** — but significant engineering effort

---

## 6. Address Resolution — What's Used

### How addresses flow through the system:

| Stage | Format | Source |
|-------|--------|--------|
| Discovery | `local-ip:port` | UDP broadcast sender IP + announced port |
| Rendezvous registration | `public-ip:port` or `?:port` | STUN-derived public IP |
| Rendezvous lookup | `ip:port` strings | HTTP response from rendezvous server |
| Connection dial | `ip:port` string | Passed to `quic.DialAddr()` or `net.Dialer` |
| Connection manager key | `ip:port` string | Used as map key in `connections` |

**No hostname resolution anywhere.** The system uses raw IP addresses exclusively. There's no DNS lookup, no mDNS hostname resolution, and no support for domain names in the `--rendezvous` URL beyond what Go's HTTP client does internally.

**The `?:port` fallback** (`main.go:223`) is a serious issue — if STUN fails, the peer registers an unusable address. Other peers will try to connect to `?:5000` which will fail.

---

## 7. Hole-Punching Logic

**There is NO hole-punching logic in the codebase.**

The connection flow is strictly sequential:
1. Discover peer address (via LAN broadcast or rendezvous HTTP)
2. Dial directly to that address
3. If dial fails -> connection fails, no retry, no alternative strategy

There's no:
- Simultaneous open coordination
- NAT type detection
- Fallback to alternate addresses
- Connection retry with different strategies

---

## 8. Practical Steps to Go from Local-Only to Internet-Capable

### Phase 1: Rendezvous Server (1-2 days)
1. **Deploy a rendezvous HTTP server** — simplest possible implementation:
   - In-memory map: roomName -> []peerAddr
   - POST /rooms/:name -> add peer to map
   - GET /rooms/:name -> return peer list
   - Auto-expire peers after 60s of no registration
2. Host it on a publicly accessible server (VPS, cloud function, etc.)
3. This alone enables WAN peer **discovery** — peers can find each other's public addresses.

### Phase 2: Public IP Registration Fix (half day)
1. Fix the `?:port` fallback in `main.go:223-226`
2. If STUN fails, don't register — log a warning and skip rendezvous registration
3. Or: use an HTTP-based IP discovery fallback (e.g., `https://api.ipify.org`)
4. Consider registering **multiple addresses** (local + public) so LAN peers can still connect directly

### Phase 3: TLS Certificate SANs (half day)
1. Add the public IP to the certificate SANs when it's known
2. Or: accept that `InsecureSkipVerify: true` is sufficient for TOFU-based security
3. Consider adding a hostname field to the rendezvous registration for DNS-based connections

### Phase 4: UPnP Port Forwarding (1-2 days)
1. Add `github.com/huin/goupnp` or similar library
2. On startup, attempt to forward the QUIC/TCP port on the router
3. If successful, use the public IP + forwarded port for rendezvous registration
4. If UPnP fails, fall back to manual port forwarding instructions for the user

### Phase 5: UDP Hole-Punching (2-3 days)
1. Modify the rendezvous server to return each peer's **server-observed** IP:port (the address the HTTP request came from)
2. Add a hole-punching coordinator:
   - Both peers learn each other's NAT-mapped addresses
   - Both send UDP packets simultaneously to those addresses
   - Once NAT mappings are created, QUIC connections can succeed
3. This requires careful timing and retry logic

### Phase 6: TURN Relay Fallback (2-3 days)
1. Deploy a TURN server (e.g., `coturn` on a VPS)
2. Add TURN client logic to the messenger
3. When direct connection fails after N attempts, fall back to TURN relay
4. This guarantees connectivity even through symmetric NATs

### Phase 7: Full ICE (1-2 weeks, optional)
1. Implement full ICE candidate gathering and exchange
2. Use the rendezvous server as a signaling channel
3. Perform connectivity checks and select best path
4. This is the most robust but most complex solution

---

## Files

| File | Role in WAN Support |
|------|---------------------|
| `discovery.go` | LAN-only UDP broadcast discovery — useless for WAN |
| `rendezvous.go` | HTTP rendezvous client — WAN-ready but needs external server |
| `security.go` | TLS cert generation — SANs limited to local IPs |
| `connection_manager.go` | Direct dial only — no NAT traversal, no retry strategies |
| `main.go` | STUN IP discovery, rendezvous registration loop, `--rendezvous` flag |
| `server.go` | Listens on `0.0.0.0:0` — already accepts external connections if port is reachable |
| `transport.go` | Transport abstraction — no WAN-specific logic |
| `rendezvous_test.go` | Tests the rendezvous client against mock HTTP servers |

---

## Red Flags

1. **No rendezvous server in repo** — The entire WAN discovery mechanism depends on an external HTTP server that doesn't exist in this codebase. Without it, `--rendezvous` is a no-op.

2. **`?:port` fallback is broken** — When STUN fails (common behind symmetric NATs or restrictive firewalls), the peer registers `?:<port>` as its address. Other peers will try to connect to this literal string and fail.

3. **No port forwarding** — The server listens on a random port (`0.0.0.0:0`). Even if a peer knows your public IP, the port is ephemeral and not forwarded through any NAT.

4. **TLS certs don't include public IPs** — Certificates are generated with only local interface IPs. While `InsecureSkipVerify: true` masks this, it means there's no cryptographic identity validation for WAN peers.

5. **No connection retry or fallback** — If a direct dial fails, the connection is abandoned. No retry with alternate addresses, no fallback to relay, no hole-punching attempt.

6. **Discovery broadcasts room names in plaintext** — Even on LAN, any device on the network can see room names. On WAN via rendezvous, room names are exposed in HTTP URLs (`/rooms/<roomName>`).

7. **No rate limiting on rendezvous** — The rendezvous server (when built) would need rate limiting to prevent abuse, as there's no authentication.

8. **QUIC over UDP may be blocked** — Many corporate firewalls block non-HTTP/HTTPS UDP traffic. The `--use-tcp` flag provides a fallback, but TCP+TLS on port 443 would be needed for reliable traversal.

---

## What I Couldn't Find

- **No rendezvous server implementation** — I searched the entire repo including `scripts/`, `cloud-init/`, and `docs/`. The server is assumed to exist externally but is not provided.
- **No configuration file** — All settings are CLI flags or hardcoded constants. There's no `.env`, `config.yaml`, or similar for storing rendezvous server URLs.
- **No NAT type detection** — The app doesn't attempt to determine whether it's behind a cone NAT, symmetric NAT, or no NAT at all.
- **No persistent peer cache** — Discovered peers are lost on restart. There's no local storage of previously-connected peer addresses.

---

## Follow-up Questions

1. **Do you want me to build the rendezvous server?** A minimal HTTP server in Go would be ~100 lines and could be added to this repo. It would handle room registration, peer listing, and TTL-based expiry.

2. **Should I implement UPnP port forwarding?** This is the highest-impact single feature for WAN connectivity — it would allow most home users to connect without manual router configuration.

3. **Is the threat model okay with `InsecureSkipVerify: true`?** The current TOFU pinning provides MITM protection after the first connection, but the initial connection has no certificate validation. For a messaging app, this may be acceptable, but it's worth confirming.
