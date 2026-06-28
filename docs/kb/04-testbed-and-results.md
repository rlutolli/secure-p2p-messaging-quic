# Test Bed, Results, and the Two Big Bugs

Full narrative: `docs/quic-vs-tcp-study-20260602.md`. Root-cause debug write-up:
`docs/debug-benchmark-results-20260601.md`. LAN/WAN crossover + adaptive design:
`docs/adaptive-transport-analysis.md`. This file is the condensed, citable summary.

## WAN test bed (the headline study)

| Role | Location | Host | Instance |
|---|---|---|---|
| Relay (server) | EU, Frankfurt | `63.185.9.233:41748` | AWS t3.large, 8 GB |
| Client (load generator) | US, Virginia | `52.7.165.55` | AWS t3.large, 8 GB |

- Cross-region AWS WAN, base RTT ≈ **90 ms**.
- Relay launched with `--relay --no-upnp --disable-gso --disable-ecn --relay-port 41748`,
  UDP buffers tuned (`scripts/apply_sysctl.sh`).
- Relay built with the per-connection outbound-queue fix so neither protocol collapses.
- All message-bearing tests: **100% delivery, 0 errors, 0 disconnects** unless noted.

### SSH access (from the user's notes)
- EU relay: `ssh -i ~/Downloads/reismac.pem ubuntu@ec2-63-185-9-233.eu-central-1.compute.amazonaws.com`
- US client: `ssh -i ~/Downloads/reismac2.pem ubuntu@ec2-52-7-165-55.compute-1.amazonaws.com`
- Relay launch (survives SSH disconnect):
  ```bash
  ssh -f ... "cd ~/secure-p2p-messaging-quic && : > relay.log && \
    setsid bash -c 'tail -f /tmp/relay_in | ./p2p-messenger --relay --no-upnp \
    --disable-gso --disable-ecn --relay-port 41748 > relay.log 2>&1' \
    </dev/null >/dev/null 2>&1"
  ```
- macOS (local dev box) has **no `timeout` command**.

## Results (WAN, exact)

### Dim 1 — Connection setup (dial avg ms)
| n | QUIC | TCP |
|---|---|---|
| 10 | 94 | 184 |
| 50 | 102 | 187 |
| 100 | 102 | 194 |
| 200 | 113 | 206 |
| 500 | 92 | 182 |
→ **QUIC ~2× faster** (1 RTT vs 2 RTT), stable across scale.

### Dim 2 — Latency by size (p50/p95 ms)
| Size | QUIC | TCP |
|---|---|---|
| 64 B | 90/91 | 90/91 |
| 256 B | 90/90 | 90/90 |
| 1 KB | 90/91 | 90/90 |
| 4 KB | 91/91 | 90/90 |
| 16 KB | 196/270 | 272/361 |
| 64 KB | 449/553 | 632/723 |
→ **tie ≤4KB, QUIC ~30% better at 64KB.**

### Dim 3 — Multistream (total elapsed ms)
| Streams | QUIC | TCP | QUIC advantage |
|---|---|---|---|
| 4 | 184 | 275 | 33% |
| 8 | 183 | 276 | 34% |
| 16 | 194 | 277 | 30% |
| 32 | 225 | 280 | 20% |
→ **QUIC wins wide** (per-stream ~91 ms vs TCP ~274 ms).

> **Provenance (VERIFIED 2026-06-05 by SSH).** Dimension 3 is the **only** one that used
> `raw_mini`, and it was a **separate manual run between the two AWS hosts** (raw echo server
> on EU — `tcp_raw.go` / `quic_diagnostic.go` — with `multiplex_bench` as the US client). It
> was **not** produced by the orchestrator: `run_full.sh`'s multistream step failed (it
> pointed `multiplex_bench` at the relay, which is ALPN/framing-incompatible — its
> `mplex_*.log` are all errors). `multistream.csv` was assembled by an uncommitted wrapper
> around `multiplex_bench`'s text output (4/8/16/32 sweep); only the CSV was copied to local.
> The ~91/274 ms values are genuine ~90 ms EU↔US RTT measurements (QUIC ≈1 RTT/stream, TCP
> ≈3 RTT/conn) — defensible, but note for the viva that this is a *raw-transport*
> micro-benchmark (not through the app relay), which is exactly what this dimension isolates.

### Dim 4 — Scale (delivery / p50 RTT)
| n | QUIC | TCP |
|---|---|---|
| 10 | 100% / 90 | 100% / 90 |
| 50 | 100% / 90 | 100% / 172 |
| 200 | 100% / 92 | 100% / 154 |
| 500 | 100% / 96 | 100% / 90 |
| 1000 | 100% / 532 | 100% / 381 |
→ **both reach 1000 @ 100% after the fix**; TCP marginally lower RTT at n=1000.

### Dim 5 — Throughput (sustained / delivery), n=50
All rates 1/5/10/50/100 msg/s: **both 100% sustained, 100% delivery.** Tie.

### Hypotheses vs outcome
| Hypothesis | Expected | Result |
|---|---|---|
| Faster connection setup | QUIC | ✅ ~2× |
| Small-message latency | tie/TCP | ✅ tie ≤4KB |
| Parallel streams | QUIC big | ✅ 20–34% |
| More peers before degrading | QUIC | ➖ tie (both 1000 @ 100%) |
| Higher sustained throughput | QUIC | ➖ tie in tested range |

## LAN behaviour and the crossover (the nuance examiners will probe)

On **loopback/LAN** the large-message result **inverts**:

Relay message RTT by size (loopback, n=4, GSO on):
| Size | QUIC p50 | TCP p50 | Winner |
|---|---|---|---|
| ≤1 KB | 0 ms | 0 ms | tie |
| 16 KB | 2 ms | 0 ms | TCP |
| 64 KB | 5 ms | 1 ms | TCP ~5× |
| 256 KB | 15 ms | 2 ms | TCP ~7× |
| 1 MB | 94 ms | 10 ms | **TCP ~9×** |

Raw 10 MB one-way bulk (loopback, no relay): TCP+TLS ~1.9 ms vs **QUIC ~150 ms (~80×)**.

> **CORRECTION (2026-06-09, `docs/lan-bulk-investigation-20260609.md`):** the ~80× is largely a
> measurement artifact. Linux re-test: the original QUIC tool used a **0-RTT** connection
> (throttled by anti-amplification → ~134 ms at **3% CPU**, idle); a normal **1-RTT** conn does
> 10 MB in **~32 ms**. The TCP baseline was **plaintext** (~5 ms) not TCP+TLS (~9 ms). Fair,
> encrypted, normal-conn: **QUIC ~32 ms vs TCP+TLS ~9 ms ≈ 3.5×**, QUIC CPU-bound (86%). Use
> **~3.5×** as the honest LAN bulk figure; the crossover (latency-bound WAN vs CPU-bound LAN)
> still holds. GSO gave only ~11% (the 0-RTT path was paced, not syscall-bound) — both research
> briefs' "GSO recovers 75–85%" did not hold here.

**Why:** WAN is *latency-bound* → QUIC's structural RTT savings win even at 64 KB. LAN is
*CPU-bound* (RTT≈0) → TCP rides kernel/NIC offload (TSO/GRO) while userspace quic-go pays to
packetise + encrypt + syscall each datagram. **The decision variable is latency-bound vs
CPU-bound, not payload size per se.** macOS exposes this more (limited GSO); Linux+GSO
narrows it but TCP still wins on a zero-loss LAN.

**Takeaway:** QUIC is the right default for a *messaging* app (small messages on real
networks). A future large-file/media path running mostly on LAN would be better on TCP (or
QUIC with OS-level GSO). This motivates the **adaptive transport** proposal (KB ref:
`docs/adaptive-transport-analysis.md`).

## The two bugs that were actually one (critical to the story)

The handed-down brief claimed **two** separate bugs: (1) throughput mode = 0% delivery,
(2) scale degrades at n>=500. Investigation proved hypothesis (1) WRONG and found a **single
root cause** behind both symptoms.

### Bug A — WAN 0% delivery (GSO/ECN)
QUIC handshake succeeded but sustained data transfer got 0% over the cross-region path,
because the path silently dropped GSO-coalesced / ECN-marked UDP datagrams. **Fix:**
`--disable-gso --disable-ecn` (env vars read at socket creation). Git: `31e67a3`,
merged `73473c9`/`416e6c8`. (See KB 03 for the mechanism.)

### Bug B — relay collapse at scale (the real, project-side root cause)
`broadcastToRoom()` ran on the **per-peer reader goroutine** and (old version) spawned a
goroutine per recipient then `wg.Wait()`-ed for all writes to finish. While blocked in that
fan-out, the reader **could not process incoming PONGs**. The 5 s health check then saw
"no PONG" and tore down peers that were actually alive → cascading false disconnects →
total delivery failure at n>=500. Throughput mode showed 0% only because it ran **last**,
after a scale n=1000 run had already wrecked the relay — *not* an independent throughput bug.

**Fix:** per-connection **bounded outbound queue + dedicated `writeLoop` goroutine**.
`broadcastToRoom` now does an O(1) non-blocking `enqueue` per peer; a slow peer only backs up
(and ultimately drops from) its **own** queue and can never block the reader or the rest of
the room. Writes are serialised per connection (also fixes unsafe concurrent QUIC stream
writes). Constants: `OutboundQueueSize=1024`. Files: `server.go`, `connection_manager.go`,
`performance.go`, `relay_test.go`.

**Verified on real WAN:** scale QUIC n=1000 0%→100%; TCP n=1000 467 errors→0; throughput all
rates 0%→100%. Documented in `debug-benchmark-results-20260601.md` §9. All `go test` /
`go test -race` pass.

> **Dissertation framing rule:** Bug B is *this project's own* design flaw and is a
> legitimate engineering-narrative centrepiece (problem → diagnosis → fix → re-measure). Bug
> A (GSO/ECN) is an *environment/library interaction*, not a general flaw — present it as a
> WAN deployment lesson, not as "QUIC is broken."
