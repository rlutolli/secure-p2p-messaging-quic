# LAN bulk investigation — the "80× gap" decomposed (2026-06-09)

**Trigger:** two deep-research briefs claimed the LAN QUIC bulk gap was mostly a macOS/no-GSO
artifact and would close to ~1.2–2× on Linux with GSO. We tested it on a Linux AWS box
(loopback, 10 MB one-way). The briefs were **wrong about the cause** — but the investigation
revealed the original "80×" headline was itself substantially a **measurement artifact**.

## Measured (Linux, AWS t3, loopback, 10 MB one-way, x3 each)

| Transport / config | Time | Throughput | Sender CPU | Notes |
|---|---|---|---|---|
| TCP (plaintext) | ~5 ms | ~2000 MB/s | 100% | no encryption (the original LAN baseline) |
| TCP + TLS 1.3 | ~9 ms | ~1100 MB/s | — | fair encrypted baseline |
| **QUIC 1-RTT (normal conn)** | **~32 ms** | **~310 MB/s** | **86%** | CPU-bound — genuine userspace cost |
| QUIC 1-RTT warm (2nd 10 MB same conn) | ~28 ms | ~360 MB/s | — | slow-start is minor (32→28) |
| QUIC 0-RTT (`quic_bench`, original tool) | ~134 ms | ~75 MB/s | **3%** | throttled + idle (NOT CPU-bound) |
| QUIC 0-RTT, GSO disabled | ~150 ms | ~67 MB/s | — | ≈ our original macOS number |

## What the original "~80×" (1.9 ms TCP vs ~150 ms QUIC) actually was

It compounded **three** separate effects, only one of which is a real transport cost:

1. **0-RTT throttle (~4×).** Our original tool (`quic_bench`) measures a **0-RTT resumed**
   connection (`DialAddrEarly`). 0-RTT data is limited by QUIC anti-amplification (server may
   send ≤3× bytes received until the address is validated) and a conservative initial flow, so
   a 10 MB 0-RTT send is throttled to ~75 MB/s with the **sender 97% idle (3% CPU)** — it is
   *waiting*, not computing. A normal **1-RTT** connection (same server, same 10 MB, same
   config) does it in ~32 ms (~310 MB/s) at **86% CPU**. So ~4× of the gap was "we benchmarked
   0-RTT bulk," which QUIC deliberately rate-limits.
2. **Plaintext-vs-encrypted (~1.8×).** The original TCP baseline was **plaintext** TCP (~5 ms);
   the fair encrypted baseline TCP+TLS 1.3 is ~9 ms. ~1.8× of the gap was "QUIC encrypts, that
   TCP didn't."
3. **Genuine userspace cost (~3.5×).** With both effects removed — fair, encrypted, normal
   connections — **QUIC ~32 ms vs TCP+TLS ~9 ms ≈ 3.5×**, and QUIC is genuinely **CPU-bound**
   here (86%). This residual is the real userspace per-packet/per-byte processing cost (AEAD +
   header protection + framing + loss/ACK bookkeeping that TCP does in-kernel/offloaded).

So the honest LAN result is **~3.5× (encrypted, normal conn), not 80×**. The 80× was 0-RTT
throttling × plaintext-TCP × the real ~3.5×.

## On GSO (where both briefs put their money)
GSO mattered little here: on the 0-RTT path it gave ~11% (150→134 ms) because that path is
**paced/idle, not syscall-bound** (3% CPU — there are no syscalls to amortise). On the 1-RTT
path the sender is CPU-bound (86%), so GSO/batching *would* help more there, but it cannot
explain or close a 27× gap — because most of that gap was never CPU/syscalls. **The briefs'
"GSO recovers 75–85%" prediction does not hold for this workload/operating point.** (GSO is
still the right lever at multi-Gbps where the CPU genuinely saturates on syscalls — a different
regime than our loopback bulk.)

## Implications for the dissertation (important — corrects earlier claims)
- The "QUIC ~80× slower on a clean LAN" line (in the original study doc and KB) must be
  **re-stated**: it conflated a 0-RTT throttle and a plaintext-TCP baseline with the real cost.
- Corrected claim: *on a clean LAN, fair encrypted bulk, normal (1-RTT) connections, QUIC is
  ~3.5× slower than TCP+TLS, and that residual is genuine userspace per-byte CPU cost (QUIC is
  CPU-bound at 86%). The often-cited huge gaps are partly measurement artifacts — 0-RTT bulk is
  deliberately rate-limited by anti-amplification, and comparisons against plaintext TCP omit
  the crypto term.*
- This is a stronger, more sophisticated result than the original and shows the crossover
  (latency-bound WAN → QUIC; CPU-bound LAN → TCP) still holds, but the LAN magnitude is ~3.5×,
  not ~80×.
- It also yields a real engineering note: **never benchmark bulk over a 0-RTT connection** —
  0-RTT is for low-latency *first-byte*, not throughput; anti-amplification throttles it.

## Tools (committed for reproducibility)
- `raw_mini/bulk_warm.go` — QUIC 1-RTT cold-then-warm 10 MB on one connection (CPU/pacing test).
- `raw_mini/tcp_tls_bench.go` — fair TCP+TLS 1.3 10 MB one-way bulk.
- Existing `raw_mini/quic_bench.go` (0-RTT) and `tcp_bench.go` (plaintext) — the original tools.
- Repro: build each, run servers on loopback (QUIC 9001 ALPN "bench", TCP 9000, TLS 9002),
  run clients; `/usr/bin/time -v` for CPU%.
