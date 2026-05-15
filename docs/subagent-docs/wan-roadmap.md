# WAN / Internet Connectivity Roadmap — Hybrid NAT Traversal

## Executive Summary

The application is currently LAN-only. This document provides a concrete,
phased roadmap to enable internet-wide peer-to-peer connectivity using a
**hybrid approach**:

1. **Auto-detect if the host already has a public IP** (no NAT)
2. **Try automatic port mapping** (UPnP/NAT-PMP) for home routers
3. **Use UDP hole punching** (ICE) for cone NATs — no port forwarding needed
4. **Fall back to free TURN relay** for symmetric NATs and blocked networks

This gives **~90-95% connectivity** without requiring users to manually
configure their routers.

---

## Hybrid Strategy: Try Everything in Parallel

Do NOT serially guess methods. Start gathering candidates in parallel and
let ICE pick the best path.

```
Priority order (ICE handles this automatically):
1. Host candidates (LAN IPs, public IPv4/IPv6)
2. UPnP/NAT-PMP mapped port (auto port forwarding)
3. STUN server-reflexive candidates (hole punching)
4. TURN relay over UDP (free fallback)
5. TURN relay over TCP/443 (firewall-bypass fallback)
```

**Key insight**: ICE (`pion/ice`) does all of this for you. You just need
to configure it with the right STUN/TURN servers and a signaling channel.

---

## Free TURN Servers Available (2024-2025)

### ⚠️ Important Warning

Free public TURN is scarce because it relays real bandwidth. Static
credentials are often abused, rate-limited, or retired. For production,
use your own `coturn` server or a provider with per-app credentials.

### Option A: Open Relay Project / Metered (Free Tier)

**Website**: https://www.metered.ca/tools/openrelay/

**Free plan**: 20 GB/month TURN usage

**Static credentials** (legacy/testing only — may require account now):
```
Username:   openrelayproject
Credential: openrelayproject

STUN:  stun:openrelay.metered.ca:80
TURN:  turn:openrelay.metered.ca:80
TURN:  turn:openrelay.metered.ca:443
TURN:  turn:openrelay.metered.ca:80?transport=tcp
TURNS: turns:openrelay.metered.ca:443?transport=tcp
```

**Account-based setup** (recommended):
1. Sign up at https://www.metered.ca/
2. Get API key
3. Fetch dynamic credentials:
```bash
curl "https://yourapp.metered.live/api/v1/turn/credentials?apiKey=YOUR_API_KEY"
```
4. Use returned `username`/`credential` in ICE config

### Option B: Cloudflare Realtime TURN

**Addresses**:
```
STUN:  stun:stun.cloudflare.com:3478
STUN:  stun:stun.cloudflare.com:53

TURN:  turn:turn.cloudflare.com:3478?transport=udp
TURN:  turn:turn.cloudflare.com:53?transport=udp
TURN:  turn:turn.cloudflare.com:3478?transport=tcp
TURN:  turn:turn.cloudflare.com:80?transport=tcp
TURNS: turns:turn.cloudflare.com:5349?transport=tcp
TURNS: turns:turn.cloudflare.com:443?transport=tcp
```

**Note**: Free when used with Cloudflare Realtime SFU. Otherwise
pay-per-GB. Requires Cloudflare account + API-generated credentials.

### Option C: Self-Hosted coturn (Recommended for Production)

A $5/month VPS can host your own TURN server.

**Minimal config** (`/etc/turnserver.conf`):
```conf
listening-port=3478
tls-listening-port=5349
realm=turn.yourdomain.com
lt-cred-mech
user=appuser:strong-password-here
external-ip=YOUR_PUBLIC_IPV4
fingerprint
no-multicast-peers
no-loopback-peers
min-port=49160
max-port=49200
```

**Firewall**:
```bash
ufw allow 3478/udp
ufw allow 3478/tcp
ufw allow 5349/tcp
ufw allow 49160:49200/udp
```

### Free STUN Servers (for discovery, NOT relay)

```
stun:stun.l.google.com:19302
stun:stun1.l.google.com:19302
stun:stun2.l.google.com:19302
stun:stun3.l.google.com:19302
stun:stun4.l.google.com:19302
stun:stun.cloudflare.com:3478
```

---

## Success Rates by NAT Type

| NAT Type | Prevalence | UPnP | Hole Punching | TURN |
|----------|-----------|------|---------------|------|
| Public IP / No NAT | ~5% | N/A | N/A | Works |
| Full Cone | ~10% | Works | **~93%** | Works |
| Restricted Cone | ~30% | Works | **~93%** | Works |
| Port-Restricted Cone | ~40% | Works | **~93%** | Works |
| Symmetric NAT | ~20% | Works | **Fails** | **Works** |
| CGNAT (mobile) | ~15% | Fails | Low-Med | **Works** |
| Corporate (UDP blocked) | ~5% | Fails | Fails | **TCP/443 Works** |

**Key numbers**:
- Tailscale reports **>90%** direct NAT traversal success with hybrid approach
- IPFS/libp2p large study (4.4M attempts): **70% ± 7.1%** hole punching,
  97.6% first-attempt success when prerequisites met
- UDP hole punching alone: **~93.67%** for cone NATs (older study of 104 routers)
- UPnP success rate: **~38%** (many routers disable it)

**Bottom line**: With TURN as fallback, you get **~95%+ connectivity**.

---

## Concrete Go Implementation

### Dependencies

```bash
go get github.com/pion/ice/v4
go get github.com/pion/stun/v3
go get github.com/pion/turn/v4      # if self-hosting TURN server
go get github.com/huin/goupnp       # UPnP port mapping
go get github.com/jackpal/go-nat-pmp # NAT-PMP
go get github.com/libp2p/go-nat     # Optional: unified NAT abstraction
```

### Step 1: Create the ICE Agent

```go
package natquic

import (
    "crypto/tls"
    "errors"
    "time"

    quic "github.com/quic-go/quic-go"
    "github.com/pion/ice/v4"
    "github.com/pion/stun/v3"
)

func mustURI(raw string) *stun.URI {
    u, err := stun.ParseURI(raw)
    if err != nil {
        panic(err)
    }
    return u
}

// NewICEAgent creates an ICE agent with STUN + optional TURN.
func NewICEAgent(turnUser, turnPass string) (*ice.Agent, error) {
    urls := []*stun.URI{
        mustURI("stun:stun.l.google.com:19302"),
        mustURI("stun:stun.cloudflare.com:3478"),
    }

    if turnUser != "" {
        // Free Metered TURN (account-based credentials)
        urls = append(urls,
            mustURI("turn:"+turnUser+":"+turnPass+"@standard.relay.metered.ca:80?transport=udp"),
            mustURI("turn:"+turnUser+":"+turnPass+"@standard.relay.metered.ca:443?transport=tcp"),
            mustURI("turns:"+turnUser+":"+turnPass+"@standard.relay.metered.ca:443?transport=tcp"),
        )
    }

    return ice.NewAgentWithOptions(
        ice.WithUrls(urls),
        ice.WithNetworkTypes([]ice.NetworkType{
            ice.NetworkTypeUDP4,
            ice.NetworkTypeUDP6,
        }),
        // Delay TURN relay nomination so direct paths are preferred
        ice.WithRelayAcceptanceMinWait(1500*time.Millisecond),
    )
}
```

### Step 2: PacketConn Adapter for quic-go

`quic-go` needs a `net.PacketConn`. Pion ICE returns `*ice.Conn`.

```go
package natquic

import (
    "net"
    "time"

    "github.com/pion/ice/v4"
)

type ICEPacketConn struct {
    Conn *ice.Conn
}

var _ net.PacketConn = (*ICEPacketConn)(nil)

func (p *ICEPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
    n, err := p.Conn.Read(b)
    return n, p.Conn.RemoteAddr(), err
}

func (p *ICEPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
    return p.Conn.Write(b)
}

func (p *ICEPacketConn) Close() error {
    return p.Conn.Close()
}

func (p *ICEPacketConn) LocalAddr() net.Addr {
    return p.Conn.LocalAddr()
}

func (p *ICEPacketConn) SetDeadline(t time.Time) error {
    return p.Conn.SetDeadline(t)
}

func (p *ICEPacketConn) SetReadDeadline(t time.Time) error {
    return p.Conn.SetReadDeadline(t)
}

func (p *ICEPacketConn) SetWriteDeadline(t time.Time) error {
    return p.Conn.SetWriteDeadline(t)
}
```

### Step 3: Gather and Exchange Candidates

```go
type Signal struct {
    Ufrag      string   `json:"ufrag"`
    Pwd        string   `json:"pwd"`
    Candidates []string `json:"candidates"`
}

// Gather local ICE credentials and candidates.
func Gather(ctx context.Context, a *ice.Agent) (*Signal, error) {
    sig := &Signal{}

    ufrag, pwd, err := a.GetLocalUserCredentials()
    if err != nil {
        return nil, err
    }
    sig.Ufrag = ufrag
    sig.Pwd = pwd

    done := make(chan struct{})
    if err := a.OnCandidate(func(c ice.Candidate) {
        if c == nil {
            close(done)
            return
        }
        sig.Candidates = append(sig.Candidates, c.Marshal())
    }); err != nil {
        return nil, err
    }

    if err := a.GatherCandidates(); err != nil {
        return nil, err
    }

    select {
    case <-done:
        return sig, nil
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

// AddRemoteCandidates adds peer's candidates to our ICE agent.
func AddRemoteCandidates(a *ice.Agent, remote *Signal) error {
    for _, raw := range remote.Candidates {
        c, err := ice.UnmarshalCandidate(raw)
        if err != nil {
            return err
        }
        if err := a.AddRemoteCandidate(c); err != nil {
            return err
        }
    }
    return nil
}
```

### Step 4: Establish ICE Connection

```go
// ConnectICE dials or accepts an ICE connection.
// controlling=true for the peer that initiates (offerer).
func ConnectICE(
    ctx context.Context,
    a *ice.Agent,
    remote *Signal,
    controlling bool,
) (*ice.Conn, error) {
    if err := AddRemoteCandidates(a, remote); err != nil {
        return nil, err
    }

    if controlling {
        return a.Dial(ctx, remote.Ufrag, remote.Pwd)
    }
    return a.Accept(ctx, remote.Ufrag, remote.Pwd)
}
```

### Step 5: Run QUIC Over ICE

```go
// QUICClientOverICE creates a QUIC client using an ICE-selected path.
func QUICClientOverICE(
    ctx context.Context,
    iceConn *ice.Conn,
    tlsConf *tls.Config,
) (*quic.Conn, error) {
    if tlsConf == nil {
        return nil, errors.New("tls config required")
    }

    pc := &ICEPacketConn{Conn: iceConn}
    tr := &quic.Transport{Conn: pc}

    return tr.Dial(
        ctx,
        pc.Conn.RemoteAddr(),
        tlsConf,
        &quic.Config{
            MaxIdleTimeout:  60 * time.Second,
            KeepAlivePeriod: 20 * time.Second,
            EnableDatagrams: true,
        },
    )
}

// QUICServerOverICE creates a QUIC server using an ICE-selected path.
func QUICServerOverICE(
    ctx context.Context,
    iceConn *ice.Conn,
    tlsConf *tls.Config,
) (*quic.Conn, error) {
    pc := &ICEPacketConn{Conn: iceConn}
    tr := &quic.Transport{Conn: pc}

    ln, err := tr.Listen(
        tlsConf,
        &quic.Config{
            MaxIdleTimeout:  60 * time.Second,
            KeepAlivePeriod: 20 * time.Second,
            EnableDatagrams: true,
        },
    )
    if err != nil {
        return nil, err
    }

    return ln.Accept(ctx)
}
```

### Step 6: UPnP Auto Port Mapping (Optional Enhancement)

Before ICE, try UPnP to get a direct public port:

```go
package natquic

import (
    "context"
    "fmt"
    "net"

    "github.com/huin/goupnp/dcps/internetgateway2"
)

// TryUPnP attempts to map the local port via UPnP.
// Returns the external IP:port if successful, empty string otherwise.
func TryUPnP(ctx context.Context, localPort int, localIP net.IP) string {
    clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
    if err != nil {
        return ""
    }

    for _, c := range clients {
        err = c.AddPortMappingCtx(
            ctx,
            "",
            uint16(localPort),
            "UDP",
            uint16(localPort),
            localIP.String(),
            true,
            "secure-p2p-messenger",
            3600,
        )
        if err == nil {
            // Get external IP
            ip, err := c.GetExternalIPAddressCtx(ctx)
            if err == nil && ip != "" {
                return fmt.Sprintf("%s:%d", ip, localPort)
            }
        }
    }
    return ""
}
```

If UPnP succeeds, register the external address with the rendezvous server
and skip ICE for that peer (direct connection).

---

## Phased Implementation Plan

### Phase 1: Build the Rendezvous Server (2-3 days)

The existing HTTP rendezvous client is useless without a server.

```go
// POST /rooms/:name  -> register peer
// GET  /rooms/:name  -> list peers
// POST /signal/:name -> exchange ICE signals
```

Add `cmd/rendezvous/main.go` with:
- In-memory map with TTL expiry (60s)
- Rate limiting per IP
- Optional room password integration

Users self-host on a $5/month VPS.

### Phase 2: Fix Public IP Registration (0.5 days)

```go
// Fix the broken ?:port fallback
publicAddr := ""
if ip := resolvePublicIP(); ip != "" {
    publicAddr = fmt.Sprintf("%s:%d", ip, app.server.Port())
}
// Only register if we have a real IP
```

### Phase 3: Integrate ICE with quic-go (5-7 days)

1. Add `pion/ice/v4` dependency
2. Create `NATTraversalService` wrapping `ice.Agent`
3. Modify `ConnectionManager`:
   - Fetch peers from rendezvous
   - Exchange ICE signals through rendezvous
   - Use `ICEPacketConn` + `quic.Transport` for QUIC over ICE
4. Handle `GetSelectedCandidatePair()` to know if direct or relay
5. Add telemetry: direct vs relay percentage

### Phase 4: Add UPnP Fallback (2-3 days)

1. On startup, attempt UPnP port mapping
2. If successful, use external IP + port for rendezvous
3. Skip ICE for peers that can connect directly
4. Clean up mapping on shutdown

### Phase 5: Configure Free TURN (1 day)

1. Sign up for Metered free plan (20 GB/month)
2. Add TURN credentials to app config
3. ICE automatically falls back to relay when needed
4. Monitor relay usage

### Phase 6: Self-Hosted TURN (Optional, 1-2 days)

Deploy `coturn` on the same VPS as the rendezvous server.

---

## Connection Timeout Budget

For messaging (not video), this timing gives good UX:

```
0 ms:     bind UDP, start ICE gathering
0 ms:     start port mapping attempt (background)
0 ms:     start signaling
250 ms:   exchange first trickle candidates
500 ms:   begin ICE connectivity checks
1500 ms:  allow TURN/UDP nomination (if direct fails)
3000 ms:  allow TURN/TCP or TURNS/443 nomination
5000 ms:  fallback to HTTPS/WebSocket relay if available
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         App                                  │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   Discovery  │  │ Rendezvous   │  │ NAT Traversal    │  │
│  │   (LAN UDP)  │  │ Client (HTTP)│  │ Service (ICE)    │  │
│  └──────────────┘  └──────────────┘  └──────────────────┘  │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ UPnP Client  │  │  STUN Client │  │  TURN Client     │  │
│  │ (optional)   │  │  (pion/stun) │  │  (pion/turn)     │  │
│  └──────────────┘  └──────────────┘  └──────────────────┘  │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────────────────────────────────────────────┐  │
│  │              ConnectionManager / QUIC                  │  │
│  │  quic.Transport{Conn: ICEPacketConn{iceConn}}         │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

---

## References

- [Pion ICE v4](https://pkg.go.dev/github.com/pion/ice/v4)
- [Pion STUN](https://github.com/pion/stun)
- [Pion NAT behavior tool](https://pkg.go.dev/github.com/pion/stun/v3/cmd/stun-nat-behaviour)
- [quic-go Transport](https://quic-go.net/docs/quic/transport/)
- [Metered Open Relay](https://www.metered.ca/tools/openrelay/)
- [Cloudflare Realtime TURN](https://developers.cloudflare.com/realtime/turn/)
- [Tailscale NAT traversal](https://tailscale.com/blog/how-nat-traversal-works)
- [IPFS/libp2p NAT study](https://arxiv.org/abs/2604.12484)
- [RFC 4787](https://www.rfc-editor.org/rfc/rfc4787)
- [RFC 5780](https://www.rfc-editor.org/rfc/rfc5780)
- [RFC 8445 — ICE](https://www.rfc-editor.org/rfc/rfc8445)
- [RFC 8656 — TURN](https://www.rfc-editor.org/rfc/rfc8656)

(End of document)
