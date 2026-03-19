# Scenario A — Baseline Results (server-side processing time)

Captured on `vmenet0` (inter-VM switched interface on macOS).  
Decoded in Wireshark with TLS key log (`tls_keys.log`) loaded and UDP 9001 set to Decode As → QUIC.

**Methodology note:** Timestamps are measured server-side from a `tcpdump -i any` capture inside node-b.
They represent the time between the inbound message arriving at node-b's NIC and the ACK leaving node-b's NIC.
Add ~180 µs (2 × ~90 µs one-way latency) for full client RTT.

Each payload group runs 3 messages on **one fresh connection per size** (Vanilla / no optimisations).

---

## Per-message RTTs

| Size (B) | Msg | TCP+TLS (µs) | QUIC (µs) | Winner | Diff (µs) |
|----------|-----|-------------|-----------|--------|-----------|
| 64       | 1   | 69          | 134       | TCP    | 65        |
| 64       | 2   | 125         | 337       | TCP    | 212       |
| 64       | 3   | 366         | 465       | TCP    | 99        |
| **64**   | **AVG** | **187** | **312** | **TCP** | **125** |
| 1024     | 1   | 89          | 160       | TCP    | 71        |
| 1024     | 2   | 577         | 299       | QUIC   | 278       |
| 1024     | 3   | 600         | 936       | TCP    | 336       |
| **1024** | **AVG** | **422** | **465** | **TCP** | **43** |
| 5000     | 1   | 90          | 127       | TCP    | 37        |
| 5000     | 2   | 160         | 309       | TCP    | 149       |
| 5000     | 3   | 335         | 968       | TCP    | 633       |
| **5000** | **AVG** | **195** | **468** | **TCP** | **273** |

---

## Key observations

- TCP wins 8/9 message exchanges overall.
- TCP latency within a size group **grows** with message number (69 → 125 → 366 µs for 64 B). Root cause: Nagle's algorithm + delayed-ACK interaction. Each subsequent message gets held waiting for an ACK before the kernel will send the next small segment.
- QUIC latency also grows within a size group (congestion window warmup on fresh connection per size).
- QUIC's one win is 1024 B msg 2 (299 µs vs 577 µs TCP) — TCP hits a delayed-ACK penalty exactly there.
- QUIC 5000 B msg 3 spike (968 µs) is likely multi-packet congestion window behavior on a new connection.
- Overall averages: TCP 268 µs vs QUIC 415 µs — TCP ~36% faster at baseline.

---

## Root causes identified for optimisation

| Issue | Protocol | Fix |
|-------|----------|-----|
| Two-write `writeMsg` (header then payload) triggers Nagle hold | TCP | Combine into single `Write` |
| Nagle's algorithm on subsequent messages | TCP | `TCP_NODELAY` (`SetNoDelay(true)`) |
| Full TLS 1.3 handshake on every reconnection | Both | `ClientSessionCache` (TCP: session resumption; QUIC: 0-RTT) |
| Under-tuned QUIC receive windows (default ~512 KB) | QUIC | Production config: 6 MB / 15 MB / 16 MB / 64 MB windows |
| Fresh connection per size (no connection reuse) | Both | Persistent connection benchmark (Scenarios B / D) |
