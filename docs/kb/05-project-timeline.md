# Project Timeline (reconstructed from git)

Use this for the dissertation's implementation/reflection chapters: the project was built
**incrementally**, prototype → hardened relay system → benchmark harness → study. Dates are
commit dates. `git log --oneline --reverse` regenerates this.

## Phase 0 — Prototype (Nov 2025)
- `7915a4e` (2025-11-09) Initial commit.
- `114240b`–`9a40cf1` (11-21) Go module + dependencies.
- `9189d53` feat(certs): encryption logic (TLS) into the project.
- `4408444` feat(server): bare-bones server communication between clients.
- `093c9ad` feat(client): basic QUIC client logic.
- `829aa34` refactor(entrypoint): modularise main into server/client.
> **Milestone:** a minimal QUIC client/server that exchanges messages.

## Phase 1 — Discovery, rooms, structure (late Nov 2025)
- `056edaa`/`09a627b` mDNS-based LAN discovery (later replaced by UDP broadcast).
- `30cc58e` connection manager for persistent peer connections.
- `77893aa` room management in the server.
- `33aa0a8` `App` struct; `b2261b7` better TLS cert generation.
- `587239a` first testing + performance docs.
- `e9c515b` discovery filtering + `/help` `/room` commands.

## Phase 2 — v0.2 hardening, dedup, discovery fixes (Dec 2025)
- `4502312` **v0.2**: debug mode, UDP discovery, system messages, join/leave visibility.
- `23ed0c5`/`f7286fb` hash-based message **deduplication** (fix duplicate messages in mesh).
- `f9778c4`/`68a70f9` self-discovery IP extraction + over-aggressive IP filtering fixes.

## Phase 3 — QUIC perf tuning (Jan 2026)
- `48c1351` (2026-01-19) **optimise QUIC performance** + consolidate documentation
  (flow-control windows, keepalives — the `performance.go` constants).

## Phase 4 — Refactors, security, rendezvous, v0.4 (Mar 2026)
- `88cb1ed` remove unused fields, extract shared helpers, clean perf constants.
- `c8e80d3` dynamic TLS cert SANs from local interfaces, 7-day cert lifetime.
- `d0cec03` discovery: `sendBroadcast` helper, configurable port, **SO_REUSEPORT fix on
  macOS**, align version to 0.4.
- `951d4f8` server fixes: alias impersonation, empty-room leak, own-join notification,
  PING/PONG, JOIN delimiter.
- `6bce45f` connection_manager: **TOFU pinning**, unhealthy eviction, protocol sanitisation,
  PING/PONG keepalive.
- `595a792`/`44f4df1` **HTTP rendezvous** client for WAN peer discovery + `--rendezvous`,
  `--discovery-port` flags, `/myip` STUN command.

## Phase 5 — raw benchmarking tools (Mar 2026)
- `d5ec465` (2026-03-19) raw benchmarking tools (`raw_mini/`), docs, VM provisioning.

## ⚠️ Phase X — the contaminated commit (May 2026) — READ KB 06
- `38bba13` (2026-05-15) commit message mentions "acoustic handshake, Double Ratchet, C++
  crypto, Dart, Android MainActivity, APK signature verification." **Those belong to a
  DIFFERENT project and do not exist in this Go codebase.** The commit's *actual* code
  changes that are part of THIS project are the password-room / argon2id work. **Leave the
  commit as-is; never cite Double Ratchet / acoustic / C++ / Dart in the dissertation.**

## Phase 6 — password rooms, keepalive fixes, audit (May 2026)
- `0d0fc1e` (2026-05-16) fix asymmetric PING/PONG keepalive (random disconnects): add
  `ClearPing()`, fix healthCheck lock-ordering deadlock via snapshot, fix double-close,
  fix TOCTOU in empty-room deletion. **+ password-protected rooms** (argon2id, `AUTH:FAILED`,
  `HasPassword` in discovery, CLI prompts).
- `b31c956` resolve all 6 critical audit findings before release.

## Phase 7 — relay mode + UPnP (May 2026)
- `2bdccde` (2026-05-18) **`--relay` flag**: room creator becomes the message hub (star
  topology); non-relay peers connect only to the relay. `IsRelay` in discovery,
  `LookupRelays()`, relay tracking (`MarkRelay`/`GetRelayAddrs`), UPnP IGD v2 + NAT-PMP auto
  port mapping, `--no-upnp`, `TestRelayStarTopology`.
- `2e7c2d0` (2026-05-27) fix random disconnects + command parsing: TOCTOU race in
  healthCheck (re-lookup before PING write), concurrent-write fixes in broadcastToRoom and
  PONG handler, commands now strictly require `/`, SO_REUSEPORT for Darwin.

## Phase 8 — the benchmark harness + the WAN study (Jun 2026)
- `ed4e848` (2026-06-01) app-level load tester + benchmark runner + chart generator.
- `642e601` dial/latency/throughput/scale modes + comprehensive WAN runner.
- `da55a51` scale params configurable via env vars.
- `b4ae990`/`a211ea5` grace period 200ms→5s→15s for WAN RTT + broadcast catch-up.
- `31e67a3` **disable QUIC GSO/ECN to fix 0% delivery over WAN** (Bug A).
- `416e6c8`/`73473c9` merge the GSO/ECN fix.
- `e96eda0` harden loadtest/relay/orchestrator for high-scale WAN.
- `856c7af`/`f56039b` merge benchmark hardening (PR #2).
- *(Working tree, not yet committed at time of writing:)* the **per-connection
  outbound-queue relay-collapse fix** (Bug B) in `server.go`/`connection_manager.go`/
  `performance.go`/`relay_test.go`, the `compare_study.sh` orchestrator, `chart_compare.py`,
  and the `report/` dissertation pipeline.

## Narrative arc for the report
prototype QUIC chat → LAN discovery + rooms → dedup + keepalive robustness → security
(TLS/TOFU/argon2id) → WAN reach (rendezvous, STUN, UPnP) → relay/star topology → a rigorous
QUIC-vs-TCP benchmark harness → a real cross-continent WAN study → found & fixed a relay
scaling bug and a WAN GSO/ECN delivery bug → measured the LAN/WAN crossover → proposed
adaptive transport. **Iterative, problem-driven engineering.**
