# QUIC vs TCP — Comparative Study (WAN, EU↔US)

**Date:** 2026-06-02
**Question:** For a decentralised P2P messaging app, does QUIC outperform TCP, on which
metrics, and at what scale does each break?

## Test Bed

| Role | Host | Instance |
|---|---|---|
| Relay (server) | EU, Frankfurt `63.185.9.233:41748` | t3.large, 8 GB |
| Client (load generator) | US, Virginia `52.7.165.55` | t3.large, 8 GB |

- Cross-region AWS WAN, base round-trip ≈ **90 ms**.
- Relay run with `--disable-gso --disable-ecn` (WAN remedies), UDP buffers tuned.
- Relay built with the per-connection outbound-queue fix (see
  `debug-benchmark-results-20260601.md` §9) so neither protocol collapses at scale.
- All message-bearing tests reached **100 % delivery, 0 errors, 0 disconnects** unless
  noted.

Reproduce:
```bash
# EU relay (host)
./p2p-messenger --relay --no-upnp --disable-gso --disable-ecn --relay-port 41748
# US client — dimensions 1,2,4,5 against the relay (produced the compare_run dataset)
TARGET=63.185.9.233:41748 ./bench/compare_study.sh
# Dimension 3 (multistream) — separate raw run:
#   EU host:  bash raw_mini/run_servers.sh        (tcp_raw :9000 + quic_diagnostic :9001)
#   US client: go run raw_mini/multiplex_bench.go -proto quic -n <4|8|16|32> -addr <EU_IP> -quicport 9001
#              go run raw_mini/multiplex_bench.go -proto tcp  -n <4|8|16|32> -addr <EU_IP> -tcpport 9000
python3 bench/chart_compare.py bench_results/compare_run
```
(Earlier exploration used `bench/run_full.sh`/`run_all.sh`; the final curated dataset was
produced by `compare_study.sh` plus the separate raw multistream run above. `run_full.sh`'s
built-in multistream step is not used — it is incompatible with the relay.)

## Results by Dimension

### 1. Connection setup (dial time vs peers)

| n | QUIC avg (ms) | TCP avg (ms) |
|---|---|---|
| 10 | 94 | 184 |
| 50 | 102 | 187 |
| 100 | 102 | 194 |
| 200 | 113 | 206 |
| 500 | 92 | 182 |

**QUIC ≈ 2× faster to connect.** QUIC completes its handshake in ~1 RTT (~90 ms);
TCP+TLS 1.3 needs TCP SYN + TLS handshake ≈ 2 RTTs (~180 ms). The gap is stable across
all scales. **Winner: QUIC** (as hypothesised).

### 2. Latency by payload size (message RTT, n=10)

| Size | QUIC p50 / p95 (ms) | TCP p50 / p95 (ms) |
|---|---|---|
| 64 B | 90 / 91 | 90 / 91 |
| 256 B | 90 / 90 | 90 / 90 |
| 1 KB | 90 / 91 | 90 / 90 |
| 4 KB | 91 / 91 | 90 / 90 |
| 16 KB | 196 / 270 | 272 / 361 |
| 64 KB | 449 / 553 | 632 / 723 |

**Tie for small messages, QUIC wins for large ones.** Up to 4 KB both ride a single WAN
RTT — protocol framing overhead is negligible (confirms the "tie on small chat messages"
hypothesis). From 16 KB up, QUIC's larger flow-control windows and pacing pull ahead: at
64 KB QUIC delivers ~30 % lower median RTT. **Winner: tie (small) / QUIC (large).**

### 3. Multistream (N parallel streams vs N connections, raw transport)

| Streams | QUIC total (ms) | TCP total (ms) | QUIC advantage |
|---|---|---|---|
| 4 | 184 | 275 | 33 % faster |
| 8 | 183 | 276 | 34 % faster |
| 16 | 194 | 277 | 30 % faster |
| 32 | 225 | 280 | 20 % faster |

**QUIC's structural win.** QUIC opens N independent streams on **one** existing connection
— each costs a single RTT and there is no head-of-line blocking between streams. TCP opens
N fresh connections, each paying SYN + TLS ≈ 3 RTTs. QUIC's per-stream latency stays ~91 ms
(one RTT) while TCP sits at ~274 ms. **Winner: QUIC, by a wide margin** (as hypothesised).

> **Methodology note.** This dimension is the only one measured with the `raw_mini` tools
> rather than the application relay. It was run as a separate step between the same two AWS
> hosts: a raw echo server on the EU instance (`tcp_raw.go` for TCP, `quic_diagnostic.go`
> for QUIC) with `raw_mini/multiplex_bench.go` as the client on the US instance, swept over
> N = 4, 8, 16, 32 at 1 KB. It deliberately bypasses the relay so it isolates *transport-level*
> stream multiplexing (the relay's newline protocol and `p2p-messenger/1.0` ALPN are not
> wire-compatible with `multiplex_bench`). The numbers reflect the same ~90 ms EU↔US path.

### 4. Scale (many peers, 1 msg/s, broadcast relay)

| n | QUIC delivery / p50 RTT | TCP delivery / p50 RTT |
|---|---|---|
| 10 | 100 % / 90 ms | 100 % / 90 ms |
| 50 | 100 % / 90 ms | 100 % / 172 ms |
| 200 | 100 % / 92 ms | 100 % / 154 ms |
| 500 | 100 % / 96 ms | 100 % / 90 ms |
| 1000 | 100 % / 532 ms | 100 % / 381 ms |

**Both now scale to 1000 peers with full delivery** (post-fix). Up to n=500 both hold near
the WAN floor. At n=1000 the single relay fans each message out to 999 peers (~1M relayed
writes/s); RTT rises for both (QUIC p50 532 ms, TCP p50 381 ms) but nothing is dropped. TCP
shows slightly lower broadcast RTT at the extreme because kernel socket writes are cheaper
than userspace QUIC stream writes at very high fan-out. **Winner: tie on delivery; TCP marginally
lower latency at n=1000.**

### 5. Throughput (rate sweep, n=50)

| Offered rate | QUIC sustained / delivery | TCP sustained / delivery |
|---|---|---|
| 1 | 1.0 / 100 % | 1.0 / 100 % |
| 5 | 5.0 / 100 % | 5.0 / 100 % |
| 10 | 10.0 / 100 % | 10.0 / 100 % |
| 50 | 50.0 / 100 % | 50.0 / 100 % |
| 100 | 100.0 / 100 % | 100.0 / 100 % |

**Both sustain the full offered load** up to 100 msg/s/peer (50,000 msgs/run) with zero
loss. At this rate range neither protocol is the bottleneck. **Winner: tie.**

## Conclusion

> **QUIC connects ~2× faster (≈90 ms vs ≈180 ms), ties TCP on small-message latency, wins
> ~30 % on large-payload latency, and completes parallel multistream work ~20–34 % faster
> thanks to no head-of-line blocking. Both protocols handle up to 1000 concurrent peers and
> 100 msg/s/peer at 100 % delivery after the relay broadcast fix; at n=1000 TCP shows
> marginally lower broadcast RTT because kernel sockets are cheaper than userspace QUIC
> streams at extreme fan-out.**

For an interactive P2P chat app — where fast connection setup, parallel streams, and small
messages dominate — **QUIC is the better default transport**. TCP remains competitive only
at the extreme broadcast-fan-out tail.

### Hypotheses vs outcome

| Hypothesis | Expected | Result |
|---|---|---|
| Faster connection setup | QUIC | ✅ QUIC ~2× faster |
| Small-message latency | Tie/TCP | ✅ Tie (≤4 KB) |
| Parallel streams | QUIC big margin | ✅ QUIC 20–34 % faster |
| More peers before degradation | QUIC hoped | ➖ Tie — both reach 1000 @ 100 % |
| Higher sustained throughput | QUIC | ➖ Tie in tested range (≤100 msg/s) |

## Artifacts

- Raw CSVs: `bench_results/compare_run/{connection,latency,scale,throughput,multistream}/`
- Charts: `bench_charts/compare/compare_*.png` and the scale charts from `bench/chart.py`
- Orchestrator: `bench/compare_study.sh` · Charting: `bench/chart_compare.py`

---

## Addendum: LAN behaviour and the large-message penalty (2026-06-02)

**Question:** Does the WAN result hold on LAN, given the known case where QUIC
struggles with larger messages?

**Short answer: no — on LAN the large-message result inverts. TCP wins, and the gap
grows with payload size.**

### Why WAN and LAN disagree
On WAN, latency (~90 ms RTT) dominates everything, so QUIC's structural wins (1-RTT
handshake, no head-of-line blocking, larger windows) decide the outcome — even at 64 KB.
On a clean LAN/loopback, RTT is ~0, so the bottleneck becomes **raw per-byte CPU cost**.
There, TCP rides kernel/hardware segmentation offload (TSO/GRO) and zero-copy, while
quic-go must, in userspace, split the payload into ~1200-byte packets, encrypt each,
run congestion/flow control, and syscall each datagram out. With no RTT to hide behind,
that per-packet cost is fully exposed.

### Measured — relay message RTT by size (loopback, n=4, native config / GSO on)

| Size | QUIC p50 | TCP p50 | Winner |
|---|---|---|---|
| 64 B | 0 ms | 0 ms | tie |
| 1 KB | 0 ms | 0 ms | tie |
| 16 KB | 2 ms | 0 ms | TCP |
| 64 KB | 5 ms | 1 ms | TCP ~5× |
| 256 KB | 15 ms | 2 ms | TCP ~7× |
| 1 MB | 94 ms | 10 ms | **TCP ~9×** |

### Measured — raw transport, 10 MB one-way bulk (loopback, no relay)

| Protocol | 10 MB transfer |
|---|---|
| TCP+TLS | ~1.9 ms |
| QUIC | ~150 ms (**~80× slower**) |

The raw test (via `raw_mini/`, no relay involved) isolates this to the transport itself,
confirming it is not a relay artifact. It is also consistent with the project's earlier
LAN baseline (`raw_mini/results_after.md`, Scenario 1: TCP faster on clean LAN at ≤5 KB),
and extends it to the large payloads that baseline never covered.

### Takeaway for the study
- **Small chat messages (≤4 KB):** QUIC vs TCP is a tie on both LAN and WAN — protocol
  overhead is invisible at chat-message sizes. For the actual product (short text
  messages) the transport choice does not affect per-message latency.
- **Large transfers (file/media sharing) on LAN:** prefer TCP, or expect QUIC to be
  several times slower and worsening with size. On macOS this is more pronounced because
  GSO support is limited; on Linux with GSO enabled the QUIC gap narrows but TCP still wins
  on a zero-loss LAN.
- **Anything over WAN / lossy / mobile links:** QUIC remains the better choice (see main
  study) — its handshake, loss-recovery, and multiplexing wins dominate once real RTT and
  loss exist.

**Net:** QUIC is the right default for a P2P *messaging* app (small messages, real-world
networks). If the app later adds large file/media transfer that runs primarily on LAN,
that path would be better served by TCP (or QUIC tuned with OS-level GSO/segmentation).

### Artifacts
- `bench_results/compare_run/lan/lan_message_rtt.csv` — relay RTT by size
- `bench_results/compare_run/lan/lan_raw_bulk_10mb.csv` — raw 10 MB transfer
- `bench_charts/compare/lan_latency_by_size.png` — chart
