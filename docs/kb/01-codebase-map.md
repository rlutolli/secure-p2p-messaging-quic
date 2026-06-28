# Codebase Map

Module: `github.com/...` (see `go.mod`), Go. Key external deps: `github.com/quic-go/quic-go`
(QUIC), `github.com/pion/stun/v3` (public-IP discovery), `golang.org/x/crypto/argon2`
(room key derivation). Everything in the repo root is `package main` — one binary,
`p2p-messenger`.

## Top-level layout

```
/                     core app (package main) — the messenger + relay
loadtest/             app-level load tester (the 5-dimension benchmark driver)
raw_mini/             raw-transport micro-benchmarks (QUIC vs TCP, no app logic)
bench/                orchestration shell scripts + Python charting
bench_results/        CSV outputs from benchmark runs
bench_charts/         PNG charts generated from the CSVs
docs/                 design notes, debug write-ups, the study report, this KB
report/               the dissertation (markdown -> LaTeX -> PDF pipeline)
scripts/              host tuning (sysctl for UDP buffers)
cloud-init/           VM provisioning for the AWS test bed
```

## Core application files (repo root, `package main`)

### `main.go` — entrypoint, CLI, interactive loop
- Parses flags: `--debug/-d`, `--use-tcp/-t`, `--rendezvous <url>`, `--discovery-port <n>`,
  `--relay`, `--relay-port <n>`, `--no-upnp`, `--disable-gso`, `--disable-ecn`.
- `--disable-gso` / `--disable-ecn` set env vars `QUIC_GO_DISABLE_GSO` /
  `QUIC_GO_DISABLE_ECN` that quic-go reads at socket-creation time (WAN remedy, see KB 03).
- On start: scans LAN for rooms (`DiscoverAllRooms`), shows a room picker, creates/joins a
  room, optionally fetches peers from a rendezvous server.
- `App` struct wires together `Server`, `DiscoveryService`, `ConnectionManager`.
- `initializeApp()` builds those; relay mode also tries UPnP/NAT-PMP port mapping.
- `runCLI()` is the REPL: `/help`, `/peers`, `/connect <addr>`, `/room`, `/myip`, `/exit`.
  Plain text is broadcast to the room. **Commands strictly require the `/` prefix.**
- `sendToRoom()`: a relay sends to all connected peers; a normal peer sends to its relay(s).
- `resolvePublicIP()` uses Google's STUN server.

### `server.go` — the listener / room hub (this is the "relay")
- `Server` listens on the **same port for both QUIC and TCP** simultaneously
  (`quic.ListenAddr` + `tls.Listen("tcp", ...)`).
- `setupSem` (buffered chan, `MaxConcurrentHandshakes=256`) bounds concurrent connection
  *setup* so a thundering herd of joining peers can't exhaust the accept path. Slot is held
  only during handshake/first-stream, not the connection lifetime.
- `Room` = name + optional `passwordHash` + set of `Peer`. `Peer` = addr, alias, conn, room.
- **Protocol (incoming):** `JOIN:<room>[|<alias>[|<password>]]`, `FROM:<alias>|<msg>`,
  `MSG:<msg>`, `PING`/`PONG`, or raw text. **(outgoing):** `SYSTEM:<msg>`,
  `FROM:<alias>|<msg>`, `PONG`.
- `joinRoom()`: password check uses `DeriveRoomKey` (argon2id) + `subtle.ConstantTimeCompare`
  (constant-time, no timing leak). Wrong password -> `AUTH:FAILED`.
- **`broadcastToRoom()` is the critical hot path.** It runs on the *sender's* per-peer
  reader goroutine. It now **enqueues** the message onto each recipient's bounded outbound
  queue (`mc.enqueue`) instead of writing inline. See KB 04 "the relay collapse fix" for why
  this matters — the old inline/`wg.Wait()` version starved PONG processing and caused
  cascading false-disconnects at n>=500.

### `connection_manager.go` — outgoing connection pool, health, dedup, TOFU
- `ConnectionManager` owns the map of `ManagedConnection`s, message dedup, TOFU pins,
  PING/PONG bookkeeping, relay-address tracking.
- **`ManagedConnection`** now has a bounded outbound queue: `out chan []byte`
  (`OutboundQueueSize=1024`), `done chan struct{}`, `closeOnce sync.Once`.
  - `newManagedConnection()` constructs it; `enqueue()` is a non-blocking send (drops on
    full queue / shutdown); `signalClosed()` idempotently closes `done`.
  - **`writeLoop()`** is the single goroutine that owns all writes to a connection. Drains
    `out`, writes under `mc.mu` (serialises with health-check PING writes — concurrent
    writes to a QUIC stream are unsafe). A write error tears the connection down.
- `Enqueue(addr, msg)` is the non-blocking broadcast path used by the relay.
- `getOrCreate()` dials a peer (QUIC or TCP+TLS), does the room JOIN handshake, starts a
  `readLoop` + `writeLoop`. QUIC dials use the tuned `quic.Config` (windows, 0-RTT, etc.).
- **TOFU** (Trust-On-First-Use): `VerifyPeerCertificate` stores each peer's cert SHA-256
  fingerprint on first connect; a later mismatch is rejected as possible MITM.
- `IsDuplicate()`: SHA-256 over `sender|message`, 8-byte hex key, 60 s window — suppresses
  echoes in the mesh.
- `healthCheck()` (every 5 s): sends `PING`; if a peer has an outstanding ping older than
  10 s it's declared dead and removed. Snapshots the conn map to avoid lock-ordering issues.
- `readLoop()`: parses incoming lines, answers `PING` with `PONG`, clears pending ping on
  `PONG`, formats `FROM:`/`SYSTEM:`/`MSG:` for display.

### `transport.go` — the QUIC/TCP abstraction
- `PeerConnection` interface (`Read`/`Write`/`Close`/`RemoteAddr`) lets the rest of the app
  treat QUIC and TCP identically.
- `QuicConnectionWrapper` wraps a `*quic.Stream` + `*quic.Conn`.
- `NetConnWrapper` wraps a `net.Conn` (TCP+TLS). **This abstraction is what makes the
  QUIC-vs-TCP comparison fair: identical app logic above the transport line.**

### `performance.go` — QUIC tuning constants + UDP buffer check
- Flow-control windows: stream initial 6 MB / max 16 MB; connection initial 15 MB / max
  64 MB. Max incoming streams 1000. Idle timeout 60 s, keepalive 30 s.
- `MaxConcurrentHandshakes=256`, `HandshakeSetupTimeout=30s`, `OutboundQueueSize=1024`.
- `CheckUDPBuffers()` warns on Linux if `net.core.rmem_max`/`wmem_max` < 7 MB (QUIC needs
  big UDP buffers; small buffers drop datagrams under load).

### `security.go` — TLS, certs, room-key derivation
- `generateTLSConfig()`: generates an **ephemeral self-signed ECDSA P-256 cert** at startup,
  TLS 1.3 only, ALPN `p2p-messenger/1.0`, SANs from local interfaces, 7-day validity.
  (Self-signed + TOFU is why clients use `InsecureSkipVerify` and pin instead.)
- Optional `SSLKEYLOGFILE` support for Wireshark decryption (`globalKeyLog`).
- `DeriveRoomKey(room, password)` = `argon2.IDKey(password, salt=room, t=1, m=64MiB, p=4,
  32 bytes)`. **argon2id**. Room name is the salt.

### `discovery.go` — LAN auto-discovery (UDP broadcast)
- UDP on port 19999 (configurable). Broadcasts to `255.255.255.255`, `127.255.255.255`,
  `127.0.0.1`. `SO_REUSEADDR` + `SO_REUSEPORT` (the latter fixed on macOS, see git).
- `DiscoveryMessage` JSON: type (`announce`/`query`), room, port, version, has_password,
  is_relay.
- Announces every 3 s; expires peers after 15 s. `DiscoverAllRooms()` is the startup scan;
  `LookupPeers`/`LookupRelays` query on demand. Filters self by local-address set.

### `rendezvous.go` — WAN peer discovery fallback (HTTP)
- Tiny HTTP client: `RenewRegistration` POSTs `{addr}` to `<server>/rooms/<room>`;
  `FetchPeers` GETs the peer list. For when there's no shared LAN. Used with `--rendezvous`.

### `upnp.go` — relay NAT traversal
- `TryPortMapping()` tries UPnP IGD + NAT-PMP so a relay behind a home router becomes
  reachable from the WAN. Returns an external addr + cleanup func. `--no-upnp` disables.

### `helpers.go` — small utilities
- `generateAlias()` (e.g. "SwiftFox42"), crypto-random `randInt`, `sanitiseField()`
  (strips `|`, `\n`, `\r` to keep the line protocol unambiguous / prevent injection).

### `discovery_{darwin,linux,other}.go` — per-OS `setReusePort`
- Build-tagged helpers for `SO_REUSEPORT` (differs across macOS/Linux/other).

### Tests (`*_test.go`)
- `server_test.go`, `connection_manager_test.go`, `relay_test.go` (incl.
  `TestRelayStarTopology`), `security_test.go`, `discovery_test.go`, `transport_test.go`,
  `upnp_test.go`, `rendezvous_test.go`, `healthcheck_diag_test.go`, `helpers_test.go`,
  `mock_test.go`, `relayport_test.go`. All pass under `go test` and `go test -race`.

## Benchmark / tooling files

### `loadtest/main.go` — the 5-dimension benchmark driver (SEE KB 02)
- Standalone `package main` (cannot import the app — it **re-implements** the wire protocol
  as a client). Flags: `-mode {dial,latency,throughput,scale}`, `-proto {quic,tcp}`, `-n`,
  `-rate`, `-duration`, `-size`, `-sizes`, `-rates`, `-target`, `-room`, `-password`,
  `-dial-stagger`, `-dial-retries`, `-disable-gso` (default true), `-disable-ecn` (default true).
- Categorises errors (benign / transient / real) so end-of-test teardown noise doesn't fail
  a run. `globalTracker` maps msgID -> send time for RTT. `buildPayload` pads with 'A's.

### `raw_mini/` — raw-transport micro-benchmarks (no app logic)
- `multiplex_bench.go` — **dimension 3 (multistream)**: QUIC N streams on 1 conn vs TCP N
  separate conns. `tcp_raw.go` / `quic_bench.go` — raw echo servers/clients.
  `quic_0rtt.go` — 0-RTT demo. `quic_diagnostic.go` — connectivity diagnostics.
- `simulate_wan.sh` / `simulate_network.sh` — `tc netem` loss/latency injection.
  `tune_udp.sh` — UDP buffer sysctl. `results_before.md` / `results_after.md` — LAN baselines.

### `bench/` — orchestration + charts
- `compare_study.sh` — **the master orchestrator** for the QUIC-vs-TCP study; runs all four
  app-level dimensions for both protocols and writes CSVs. (Multistream is run separately
  via `raw_mini`.) Params are env-overridable (see KB 02).
- `run_full.sh` / `run_all.sh` — earlier single-protocol scale runners.
- `chart.py`, `chart_compare.py`, `chart_lan_size.py`, `chart_wan_lan_crossover.py` —
  matplotlib charting.
- `start_relay_remote.sh` — launches the relay on the EU VPS.

## Key data-flow summary

Normal peer (relay mode off) → discovers a relay on LAN (or via rendezvous) → opens **one**
connection to the relay → `JOIN`s the room. To send: peer writes `FROM:alias|msg` to the
relay. The relay's `broadcastToRoom` **enqueues** that line onto every *other* room peer's
outbound queue; each peer's `writeLoop` flushes it. So the topology is a **star**: relay in
the centre, every message does peer→relay→all-other-peers. The relay terminates TLS, so it
sees plaintext (this is why it is transport-encryption, not E2E).
