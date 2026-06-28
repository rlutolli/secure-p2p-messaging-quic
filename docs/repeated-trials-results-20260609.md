# Repeated-Trial Results (n=5) — Baseline vs Improved

**Date:** 2026-06-09  **Test bed:** EU relay (Frankfurt) ↔ US client (Virginia), ~90 ms RTT.
**Method:** each dimension's full sweep run 5× (`bench/repeat_study.sh`), aggregated to
mean ± 95% CI (`bench/aggregate_repeats.py`, Student-t). Multistream run separately 5×
(`bench/multistream_study.sh`) against raw echo servers. **Baseline** = relay before the
code changes; **Improved** = relay with single-RLock broadcast fan-out + coalesced writes.
Both kept in separate result roots; never mixed.

## Code changes measured (the "improved" relay)
1. `broadcastToRoom`: take the connection-map RLock **once** for the whole fan-out (was once per peer → ~999 acquisitions/broadcast at n=1000).
2. `writeLoop`: **coalesce** all currently-queued messages into a single write (cuts userspace QUIC stream writes/syscalls at high fan-out).
3. TOFU first-contact hardening (persistent pin store) — security, no perf impact.

## Dimension 4 — Scale: broadcast RTT p50 (ms), both 100% delivery

| n | QUIC baseline | QUIC improved | TCP baseline | TCP improved |
|---|---|---|---|---|
| 10 | 90.0 ± 0.0 | 90.0 ± 0.0 | 90.0 ± 0.0 | 90.0 ± 0.0 |
| 50 | 90.0 ± 0.0 | 90.0 ± 0.0 | 171.2 ± 0.6 | 170.0 ± 0.9 |
| 200 | 92.0 ± 0.0 | 92.0 ± 0.0 | 154.4 ± 0.7 | 150.4 ± 2.7 |
| 500 | 98.6 ± 0.7 | 97.6 ± 0.7 | 91.0 ± 0.0 | 90.0 ± 0.0 |
| 1000 | 610.4 ± 36.1 | 266.2 ± 8.5 | 519.2 ± 55.4 | 115.2 ± 1.6 |

**Headline:** at n=1000 the broadcast-path changes cut p50 RTT **QUIC 610→266 ms (−56%)** and **TCP 519→115 ms (−78%)**. Below n=1000 the queues rarely hold >1 message, so the optimisations are inert (unchanged within CI) — exactly as expected. At extreme fan-out TCP (kernel writes) now beats QUIC (userspace writes), reinforcing the user-space-cost theme.

## Dimension 1 — Connection setup: dial avg (ms) — unchanged (sanity)

| n | QUIC baseline | QUIC improved | TCP baseline | TCP improved |
|---|---|---|---|---|
| 10 | 94.5 ± 1.3 | 94.1 ± 0.2 | 183.1 ± 0.6 | 183.0 ± 0.3 |
| 50 | 102.4 ± 5.6 | 100.5 ± 3.3 | 187.2 ± 0.6 | 187.4 ± 0.5 |
| 100 | 103.8 ± 3.1 | 103.0 ± 2.6 | 194.7 ± 0.7 | 194.1 ± 0.5 |
| 200 | 114.5 ± 6.0 | 113.0 ± 6.6 | 206.3 ± 1.8 | 205.9 ± 1.1 |
| 500 | 91.6 ± 0.3 | 91.6 ± 0.1 | 181.8 ± 0.4 | 181.6 ± 0.1 |

## Dimension 2 — Latency by size: RTT p50 (ms) — unchanged (sanity)

| size (B) | QUIC baseline | QUIC improved | TCP baseline | TCP improved |
|---|---|---|---|---|
| 64 | 90.2 ± 0.6 | 90.2 ± 0.6 | 90.0 ± 0.0 | 90.0 ± 0.0 |
| 256 | 90.2 ± 0.6 | 90.0 ± 0.0 | 90.0 ± 0.0 | 90.0 ± 0.0 |
| 1024 | 90.2 ± 0.6 | 90.0 ± 0.0 | 90.0 ± 0.0 | 90.0 ± 0.0 |
| 4096 | 91.0 ± 0.9 | 90.4 ± 0.7 | 90.2 ± 0.6 | 90.0 ± 0.0 |
| 16384 | 196.2 ± 2.2 | 196.8 ± 0.6 | 271.4 ± 1.4 | 297.2 ± 49.1 |
| 65536 | 452.0 ± 1.8 | 452.8 ± 4.1 | 559.6 ± 50.3 | 612.2 ± 48.0 |

(16–64 KB TCP shows wide CIs ~±48 ms in both runs — high-variance large-payload TCP, not a regression.)

## Dimension 5 — Throughput: all rates 1–100 msg/s/peer → 100% delivery, both protocols, both runs (tie).

## Dimension 3 — Multistream (separate raw run, n=5): total elapsed (ms)

| streams | QUIC | TCP | QUIC advantage |
|---|---|---|---|
| 4 | 183.4 ± 1.0 | 274.5 ± 0.7 | 33% faster |
| 8 | 183.5 ± 0.5 | 275.1 ± 0.7 | 33% faster |
| 16 | 193.8 ± 1.4 | 276.4 ± 0.8 | 30% faster |
| 32 | 224.1 ± 0.8 | 279.7 ± 1.2 | 20% faster |

Artifacts: `bench_results/repeats_baseline/`, `bench_results/repeats_improved/`, `bench_results/repeats_multistream_wan/` (raw per-rep CSVs + `aggregated/`).

## Loss-resilience study (tc netem on relay egress, n=3 per loss level)

Loss injected on the EU relay's egress (`ens5`) with `tc netem loss {1%,3%}`; latency-by-size
(n=10) and scale delivery measured from the US client. Clean (0%) column is the baseline run.

### Latency p95 (ms) by payload size — clean / 1% / 3% loss
| size | QUIC clean | QUIC 1% | QUIC 3% | TCP clean | TCP 1% | TCP 3% |
|---|---|---|---|---|---|---|
| 64 B | 91±0 | 91±0 | 91±1 | 90±1 | 90±0 | 90±1 |
| 1 KB | 91±1 | 91±2 | 91±0 | 90±1 | 90±0 | **120±129** |
| 16 KB | 270±1 | 270±8 | **338±20** | 361±1 | 361±1 | **450±387** |
| 64 KB | 553±3 | 616±64 | **791±146** | 703±51 | 751±129 | **961±564** |

**Reading:** small single-packet messages are unaffected by loss (one packet rarely hit).
For larger payloads under 3% loss, QUIC's p95 is both **lower and far more stable** than TCP's
— note TCP's huge CIs (±387, ±564, and a ±129 stall even at 1 KB) which are RTO stalls firing
in some repeats. QUIC degrades gracefully and predictably (tight CIs); TCP degrades worse and
erratically. This is the real-WAN confirmation of QUIC's loss recovery (RFC 9002 monotonic
packet numbers / no RTO ambiguity) vs TCP's ~200 ms minimum RTO + head-of-line blocking.

**Delivery:** both protocols held **100% delivery** at all loss levels (1% and 3% are within
retransmission's reach). So in this range loss costs *tail latency and predictability*, not
delivered messages. Scale p50 RTT was essentially unchanged by loss (low rate, 1-packet msgs).

## 0-RTT reconnection burst (WAN, raw transport, 10 sequential reconnects)

`multiplex`/raw tools: `tcp_raw -burst 10` vs `quic_diagnostic -burst 10`, 1 KB, 3 reps,
against raw echo servers on the EU host (port 41748). Connection-establishment cost per
reconnect:

| | conn 1 dial | conns 2–10 dial | 10-conn dial total |
|---|---|---|---|
| QUIC | 93 ms (1-RTT) | **0.5 ms each (0-RTT)** | **~98 ms** |
| TCP | ~182 ms | ~182 ms (no resumption) | **~1823 ms** |

**QUIC establishes the 10 reconnections ~18× faster overall, and ~360× faster per reconnect
(0.5 ms vs 182 ms).** TCP gets no resumption benefit because Go's `crypto/tls` does not
implement TLS-1.3-over-TCP 0-RTT early data. (Caveat: the QUIC 0-RTT *echo* RTT reads ~274 ms
because `quic-go`'s server processes 0-RTT application data only after the handshake completes;
the dial cost is the clean, defensible metric — it isolates connection establishment.)

Artifacts: `bench_results/repeats_loss1/`, `bench_results/repeats_loss3/` (+ `aggregated/`);
0-RTT burst raw output captured in this session.

## Transport racing (Happy-Eyeballs, implemented feature; dial mode, n=3)

`--race` (app) / `-proto race` (loadtest): dial QUIC and TCP in parallel, keep the first
handshake, cancel the loser. Dial time avg (ms), WAN:

| n | QUIC only | TCP only | Race |
|---|---|---|---|
| 10 | 92.9 ± 2.4 | 182.9 ± 0.4 | 94.5 ± 0.9 |
| 100 | 104.4 ± 21.3 | 192.9 ± 0.1 | 119.5 ± 4.3 |
| 500 | 91.8 ± 0.2 | 181.8 ± 0.1 | 91.9 ± 0.3 |

**Racing delivers QUIC-class dial latency (~92 ms) automatically — ~half of TCP's ~183 ms —
plus transparent fallback if a transport is blocked.** Mid-scale (n=100) it costs ~15 ms over
QUIC-only (2× the dials → client contention) but stays far below TCP and converges to QUIC at
n=500. This is the dissertation's proposed adaptive transport, built and measured (RFC 8305).
Code: `connection_manager.go` `raceDial`/`dialQUICPeer`/`dialTCPPeer` (+ `--race`),
`loadtest/main.go` `dialRace` (+ `-proto race`), tests in `race_test.go`. Data:
`bench_results/repeats_race_dial/race_dial.csv`. Figure: `report/figures/res_race_dial.png`.
