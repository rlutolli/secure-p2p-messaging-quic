# Adaptive Transport Selection: Analysis & Design

**Date:** 2026-06-02
**Scope:** Two research questions raised during the QUIC vs TCP study, with empirical
data and a concrete (implementable) design for automatic QUIC/TCP switching.

This document is written to feed directly into the dissertation. It separates
**measured facts** from **design proposal** and flags every assumption.

---

## Q1. Does the "large transfer" penalty hit QUIC on WAN, or only on LAN?

**Answer: It is a LAN/low-latency phenomenon. On WAN, QUIC stays ahead of TCP even
for large messages, up to at least 4 MB.**

### Measured — message round-trip via the relay, large payloads

| Size | LAN QUIC p50 | LAN TCP p50 | WAN QUIC p50 | WAN TCP p50 |
|---|---|---|---|---|
| 64 KB | 5 ms | **1 ms** | **308 ms** | 454 ms |
| 256 KB | 15 ms | **2 ms** | **601 ms** | 909 ms |
| 1 MB | 94 ms | **10 ms** | **965 ms** | 1453 ms |
| 4 MB | — | — | **1413 ms** | 1928 ms |

(LAN = loopback on an M4 Mac; WAN = EU↔US, ~90 ms base RTT. Bold = winner.)

### Interpretation
- **The crossover is driven by the latency-to-CPU ratio, not by payload size alone.**
  - On **LAN** (~0 ms RTT), there is no propagation delay to amortise per-packet work
    against. The bottleneck is raw per-byte CPU: TCP rides kernel/NIC segmentation
    offload (TSO/GRO) and zero-copy; quic-go segments, encrypts, paces, and `sendmsg`s
    every ~1200-byte datagram in userspace. QUIC loses, and the gap **widens with size**
    (5× at 64 KB → ~9× at 1 MB; raw 10 MB bulk was ~80× — see study addendum).
  - On **WAN**, propagation delay dominates wall-clock time. Both protocols spend most of
    the transfer waiting for ACKs/round-trips, which hides QUIC's per-packet CPU cost.
    QUIC's better loss recovery, larger default windows, and pacing then make it the
    *faster* option even at multi-MB sizes. TCP's advantage evaporates because its cheap
    kernel send path was never the WAN bottleneck.

### So the rule of thumb for the paper
> The transport that wins depends on **whether the link is CPU-bound or latency-bound**.
> LAN/datacenter (CPU-bound) favours TCP for bulk; WAN/mobile/lossy (latency-bound)
> favours QUIC at every size tested. Payload size only matters in combination with RTT.

**Caveat:** LAN numbers are macOS loopback where GSO support is weak; on Linux with GSO
the LAN QUIC gap narrows but does not close on a zero-loss link. A same-subnet AWS LAN
re-run with GSO on would give publication-grade LAN-bulk numbers — recommended as
future work.

---

## Q2. Can the app automatically switch between QUIC and TCP based on the link?

**Yes — and the codebase is unusually well-positioned for it.** Below is a feasibility
analysis grounded in the existing architecture, three design options ranked by
effort/value, the decision signals to use, and the pitfalls.

### Why it is feasible here (existing foundations)
1. **The relay already listens on QUIC *and* TCP on the same port simultaneously**
   (`NewServer` binds `quic.ListenAddr` and `tls.Listen` on the same `host:port`). A peer
   can therefore reach the same relay over either transport with no extra configuration.
2. **Transport is abstracted behind one interface** — `PeerConnection`
   (`Read/Write/Close/RemoteAddr`) with `QuicConnectionWrapper` and `NetConnWrapper`.
   The server and connection manager are transport-agnostic.
3. **Transport choice is currently a single static field** — `ConnectionManager.useTCP`,
   set once from the `--use-tcp` flag and read only inside `getOrCreate()`. This is the
   one place a per-connection decision would live.

So the change is: turn one process-wide boolean into a **per-connection, runtime
decision**, with a probe/feedback signal to drive it.

### What signal should drive the switch?
The Q1 result says the decision variable is the **latency-to-throughput-need ratio**, not
"LAN vs WAN" as a label. Practical, observable signals (cheapest first):

| Signal | How to obtain | Cost | What it tells us |
|---|---|---|---|
| **Measured RTT** | QUIC exposes smoothed RTT; for TCP, time a PING/PONG (the app already has PING/PONG) | ~free | latency-bound vs CPU-bound |
| **Dial-time RTT** | already measured in `dial`/loadtest as dial_ms | ~free | rough LAN vs WAN at connect |
| **Peer address class** | private RFC-1918 / link-local / same-/24 ⇒ likely LAN | ~free | coarse LAN hint |
| **Loss / retransmits** | QUIC connection stats | cheap | favours QUIC strongly |
| **Throughput need** | message size + send rate from the app layer | ~free | only large+frequent matters |
| **Active A/B probe** | open both, send a sized echo on each, compare | moderate | ground truth, but costs a probe |

**Recommended composite policy (heuristic, no probe needed for v1):**
```
if peer is on the same LAN (RFC-1918 / same /24 / link-local)
   AND the workload includes bulk transfer (size ≥ ~32 KB):
        prefer TCP
else (public/WAN/mobile peer, OR small interactive messages):
        prefer QUIC          # default — wins everywhere except LAN-bulk
```
Because chat messages are tiny, **the default is and should remain QUIC**; TCP is selected
only for the narrow "LAN + bulk file/media" case the data identifies.

### Design options (ranked)

**Option A — Static heuristic at dial time (recommended first step).**
Decide transport once per peer in `getOrCreate()` using cheap signals (peer-address class +
optional dial RTT). No ongoing measurement.
- Pros: ~50 LOC; no protocol changes; deterministic; easy to evaluate/measure for the paper.
- Cons: cannot adapt if conditions change mid-session; "is this peer on my LAN" is a
  heuristic (private IP ranges, same /24), not ground truth.
- Implementation sketch:
  - Add `func (cm *ConnectionManager) chooseTransport(peerAddr string, hintBulk bool) bool`
    returning `useTCP`.
  - In `getOrCreate()` replace the `if cm.useTCP` branch with `if cm.chooseTransport(...)`.
  - Keep `--use-tcp` / a new `--transport=auto|quic|tcp` flag to force/override.

**Option B — Adaptive with runtime feedback (the interesting research contribution).**
Start on QUIC (safe default), measure smoothed RTT + loss + observed goodput per peer; if a
peer is confirmed low-RTT *and* the session starts moving bulk data, transparently migrate
that peer's connection to TCP (and vice-versa). Migration = open the new transport, replay
the JOIN handshake, swap the `PeerConnection` in the pool, drain/close the old one.
- Pros: adapts to changing conditions (Wi-Fi→Ethernet, roaming); genuinely novel; strong
  dissertation material with before/after graphs.
- Cons: connection migration is real engineering — in-flight message ordering, dedup
  across the swap (the app already has `IsDuplicate`), avoiding flap (needs hysteresis).
- Risk to manage: **oscillation.** Require a dwell time + margin (e.g. only switch if the
  alternative is predicted ≥20% better for ≥N seconds) to prevent thrashing.

**Option C — "Happy Eyeballs"-style racing.**
Dial QUIC and TCP in parallel, keep whichever completes/echoes a sized probe first; close
the loser. Mirrors RFC 8305 (Happy Eyeballs) used by browsers for IPv4/IPv6.
- Pros: zero heuristics, picks the empirically faster path; robust to QUIC-blocking
  firewalls (a known WAN issue noted in the repo's own docs).
- Cons: doubles dial cost/connections briefly; the "fastest to connect" winner is a proxy
  for handshake speed (always QUIC) not bulk throughput — so it answers a different
  question than Q1. Best used for *reachability/connectivity*, layered with A or B for
  *throughput*.

### Recommended path for the project + paper
1. **Implement Option A** (static heuristic) — small, measurable, ships value.
2. **Prototype Option B** (RTT/loss-driven migration with hysteresis) as the research
   centrepiece — this is where the novel results and graphs come from.
3. **Mention Option C** as the connectivity-layer complement (also the natural answer to
   "QUIC/UDP is firewall-blocked → fall back to TCP/443").

### Honest pitfalls / threats to validity (include in the paper)
- **Firewall/NAT reality:** UDP (QUIC) is blocked on some networks; TCP/443 traverses more
  reliably. An adaptive layer doubles as a *connectivity* fallback, not just a perf
  optimiser — arguably its strongest real-world justification.
- **The LAN-bulk win is platform-sensitive** (GSO/offload support, NIC, kernel). Numbers
  must be reported per-environment; the *direction* generalises, the *magnitude* does not.
- **Measurement vs label:** "LAN" should be inferred from measured RTT/loss, not just
  private IPs (VPNs and overlay networks blur the line).
- **Chat workload caveat:** for the actual product (small messages) transport choice barely
  affects per-message latency — so framing the contribution around *bulk/file transfer*
  and *connectivity resilience* is the honest, defensible angle.
- **0-RTT security:** QUIC 0-RTT (used in some benches) is replay-vulnerable; do not enable
  it for non-idempotent application data without anti-replay — worth a sentence in the paper.

### Minimal experiment plan to validate an adaptive policy (future work)
1. Re-run large-payload bulk on a same-subnet AWS LAN (GSO on) for clean LAN-bulk numbers.
2. Implement Option A; measure end-to-end file-transfer time LAN vs WAN with `auto` vs
   forced-QUIC vs forced-TCP — expect `auto` to match the best of both.
3. Prototype Option B; add a controlled mid-session network change (`tc netem` to add
   delay/loss, as `raw_mini/simulate_wan.sh` already does) and show the connection migrating
   and recovering — the headline graph.

---

## One-paragraph summary for the abstract/conclusion
> The optimal transport is determined by whether the path is latency-bound or CPU-bound.
> Measurements show QUIC wins on WAN at every payload size tested (handshake, loss recovery,
> multiplexing), while on a clean LAN TCP wins for bulk transfer because QUIC's userspace
> per-packet cost is exposed once propagation delay disappears. Because the application's
> own traffic (chat) is small-message, QUIC is the correct default; an adaptive layer that
> selects TCP only for LAN-local bulk transfer — and falls back to TCP/443 when UDP is
> blocked — captures the best of both with a small, well-scoped mechanism enabled by the
> system's existing dual-stack relay and transport-abstraction interface.
