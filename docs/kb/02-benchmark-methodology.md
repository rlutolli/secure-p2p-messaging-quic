# Benchmark Methodology — the 5 dimensions (exact)

This is the precise, code-grounded description of how the QUIC-vs-TCP study was measured.
Source files: `loadtest/main.go` (driver, modes 1/2/4/5), `raw_mini/multiplex_bench.go`
(dimension 3). The **final curated dataset** (`bench_results/compare_run/`, the study doc and
charts) was produced by `bench/compare_study.sh` for dimensions 1/2/4/5 — confirmed by the
CSVs themselves (scale `duration=20`, throughput `n=50 duration=10`, latency `n=10`, Jun-2
timestamps — these are `compare_study.sh`'s defaults, not `run_full.sh`'s 30 s).
`bench/run_full.sh` and `bench/run_all.sh` were the **earlier (Jun 1) exploratory runs** that
appear in the shell histories (they used `duration=30` and `run_<timestamp>/` dirs).

## How a message and its size are built (the "argument vs characters" answer)

Message **size is controlled by an integer argument**, not by typing characters. The
`-size` flag (default **100 bytes**) sets the *target total payload length in bytes*. The
driver then **pads with the literal character `'A'`** to hit that exact byte count:

```go
func buildPayload(msgID string, size int) string {
    prefix := msgID + "|"                 // e.g. "bench-0-0|"
    if len(prefix) >= size { ... truncate ... }
    padding := size - len(prefix)
    return prefix + strings.Repeat("A", padding)   // pad with 'A' to reach `size`
}
```

So a 100-byte message = `"<msgID>|" + ("A" * (100 - len(msgID) - 1))`. The wire line that
goes out is `FROM:<alias>|<payload>\n`. (Because ASCII 'A' is 1 byte, character count ==
byte count here — there are no multi-byte runes.) **Key point for the viva:** the size is
parametric (an argument), reproducible, and identical for QUIC and TCP, so any difference
is the transport, not the payload.

`msgID` format is `"<alias>-<index>"`, e.g. `bench-12-0`. RTT is matched on this ID: the
sender records `sendTimes[msgID] = now` before writing, and the reader computes
`RTT = now - sendTimes[msgID]` when the echoed `FROM:...|msgID|...` line comes back from
the relay. RTT is measured in **whole milliseconds** (`time.Since(...).Milliseconds()`).

## Common mechanics (apply to all message-bearing modes)

- Each simulated peer is a goroutine that **dials → JOINs the room → runs a reader goroutine
  → sends → drains**. The reader answers relay `PING`s with `PONG` (so the peer stays
  "healthy") and records RTTs.
- **One shared relay broadcasts to the room.** A peer never receives its own message back —
  the relay fans a message out to *other* peers only. So you need n>=2 to measure delivery;
  the tool warns if n<2.
- After sending, every peer sleeps a **15 s drain** before closing, so in-flight WAN replies
  and high-fan-out broadcasts have time to arrive before the connection tears down. (This
  grew 200ms→5s→15s during debugging; see git `b4ae990`, `a211ea5`.)
- **GSO and ECN are disabled by default** in the load tester (`-disable-gso=true`,
  `-disable-ecn=true`) because some WAN paths silently drop GSO-coalesced / ECN-marked UDP
  datagrams, which caused 0% QUIC delivery (see KB 03/04).
- `-dial-stagger` auto-applies 5 ms between peer launches at n>=500 (3 ms in the orchestrator)
  to avoid a thundering herd against the relay accept loop. `-dial-retries` (default 3)
  retries only *transient* dial errors with exponential backoff.
- Percentiles: sorted-array, `idx = ceil(len*p/100) - 1`. avg = mean.

## Dimension 1 — Connection setup (`-mode dial`)

- **What it measures:** time to establish a connection and immediately close it
  (`dial_ms`), nothing more. Isolates handshake cost.
- **Procedure:** launch `n` peers; each times `dialWithRetry` then closes. No room join, no
  messages.
- **Parameters (study):** `n ∈ {10, 50, 100, 200, 500}`. (`DIAL_PEERS` in `compare_study.sh`.)
- **Output:** `dial_ms` min/p50/p95/p99/max/avg + errors.
- **Why QUIC should win:** QUIC carries TLS 1.3 inside its handshake → connection ready in
  ~1 RTT. TCP+TLS needs TCP SYN/SYN-ACK (1 RTT) *then* the TLS handshake (≈1 RTT) ≈ 2 RTTs.
- **Result:** QUIC ~92–113 ms vs TCP ~182–206 ms → **QUIC ≈ 2× faster**, stable across scale.

## Dimension 2 — Message latency by payload size (`-mode latency`)

- **What it measures:** round-trip time of a single message through the relay, swept across
  payload sizes.
- **Procedure:** for each size, launch `n` peers; each peer dials, joins, sends **exactly
  one** message, and waits (up to `-duration`) for its echo to come back via the relay; RTT
  recorded.
- **Parameters (study):** `sizes = {64, 256, 1024, 4096, 16384, 65536}` bytes; `n = 10`;
  `-duration 5` s. (`LAT_SIZES`, `LAT_N`, `LAT_DURATION`.)
- **Output (per size):** RTT min/p50/p95/p99/max, msgs sent/recv, errors.
- **Result:** **tie ≤ 4 KB** (both ride one ~90 ms WAN RTT; protocol overhead invisible at
  chat-message sizes); **QUIC wins from 16 KB up** (≈30% lower median at 64 KB) thanks to
  bigger flow-control windows + pacing. This is the dimension that exposes the **LAN/WAN
  crossover** (see KB 04): on LAN the large-message result *inverts* and TCP wins.

## Dimension 3 — Multistream / parallelism (`raw_mini/multiplex_bench.go`)

- **What it measures:** time to complete `N` parallel request/response exchanges — QUIC
  using **N independent streams on ONE connection** vs TCP using **N separate connections**.
  This is the head-of-line-blocking / connection-setup-amortisation dimension.
- **Procedure:** QUIC warms up once (cache a session ticket), dials one connection, then
  opens N streams concurrently; each stream sends one `size`-byte message and reads the
  echo. TCP launches N goroutines, each opening a fresh TCP+TLS connection (each paying
  SYN + TLS). `TOTAL_ELAPSED` = wall-clock from first launch to last response.
- **Parameters (study):** `N ∈ {4, 8, 16, 32}`; `size = 1024` bytes. Runs against dedicated
  raw echo servers — `tcp_raw.go` (TCP) and `quic_diagnostic.go` (QUIC, ALPN
  `quic-diagnostic`), i.e. `raw_mini/run_servers.sh` on ports 9000/9001 — **not** the app
  relay (the relay's ALPN `p2p-messenger/1.0` and newline framing are incompatible with
  `multiplex_bench`).
- **Provenance (VERIFIED 2026-06-05 by SSH into both AWS hosts):** dimension 3 was a
  **separate, manual run between the two AWS instances** — a raw echo server on the EU host,
  `multiplex_bench` as the client from the US host against EU's public IP:port. The
  ~91 ms / ~274 ms figures are the real ~90 ms EU↔US RTT (QUIC ≈ 1 RTT/stream, TCP ≈ 3
  RTT/connection). This is the **only** dimension that used `raw_mini`; the other four used
  `loadtest` against the relay. The `run_full.sh` built-in multistream step **failed and is
  NOT the source** (its `mplex_*.log` show `tls: no application protocol` for QUIC and
  `unexpected EOF` for every TCP conn, because it wrongly pointed `multiplex_bench` at the
  relay). `multistream.csv` (`per_stream_*` columns) was produced by an **uncommitted
  wrapper** that parsed `multiplex_bench`'s text output across the 4/8/16/32 sweep; only the
  CSV was copied to local — the wrapper is on neither host.
- **Length framing:** raw tools use a 4-byte big-endian length prefix + payload (different
  from the app's newline protocol). Payload built by `makePayload` — `"MPLEX<idx>:SIZE=<n>:"`
  then padded with `'A'`.
- **Result:** QUIC **20–34% faster** (QUIC per-stream stays ~91 ms = 1 RTT; TCP ~274 ms
  because each connection costs ~3 RTTs and there's no shared connection to amortise).
- **Note for the report:** this maps to the user's objective 3.4 ("multistream parallelism…
  e.g. many peers in one chatroom") — it models the cost of many simultaneous channels.

## Dimension 4 — Scale (`-mode scale`)

- **What it measures:** delivery ratio and RTT as the number of concurrent peers grows, at a
  low message rate, all broadcasting through the single relay.
- **Procedure:** launch `n` peers; each sends at `-rate` msg/s for `-duration` s; the relay
  broadcasts every message to the other n−1 peers. Track delivery (recv/expected), RTT,
  errors, disconnects.
- **Parameters (study):** `n ∈ {10, 50, 200, 500, 1000}`; `-rate 1` msg/s; `-duration 20` s;
  `-size 100` bytes. (`SCALE_PEERS`, `SCALE_RATE`, `SCALE_DURATION`, `SCALE_SIZE`.)
- **Result:** **after the relay fix, both QUIC and TCP reach 100% delivery to n=1000, 0
  errors.** RTT climbs at n=1000 (QUIC p50 532 ms, TCP p50 381 ms) because the relay does
  ~10^6 relayed writes/s fanning out; TCP's kernel-socket writes are marginally cheaper than
  userspace QUIC stream writes at that extreme fan-out. **This dimension is where the
  original "collapse at n>=500" bug lived** — see KB 04.

## Dimension 5 — Throughput (`-mode throughput`)

- **What it measures:** whether each protocol can sustain an increasing offered message rate
  at fixed peer count without loss.
- **Procedure:** for each rate, launch `n` peers each sending at that rate for `-duration` s;
  compare sustained throughput vs offered, and delivery.
- **Parameters (study):** `rates = {1, 5, 10, 50, 100}` msg/s/peer; `n = 50`; `-duration 10`
  s; `-size 100` bytes. (`THRU_RATES`, `THRU_N`, `THRU_DURATION`.)
- **Throughput maths:** `avg_throughput_msgs_per_sec = total_msgs_sent / n / duration`;
  `KB/s = total_msgs_sent * size / 1024 / n / duration`.
- **Result:** **tie** — both sustain the full offered load up to 100 msg/s/peer (50,000
  msgs/run) at 100% delivery. Neither protocol is the bottleneck in the tested range.

## The orchestrator (what produced the final data)

The **final curated dataset** in `bench_results/compare_run/` (used by the study doc and
charts) was produced by **`bench/compare_study.sh`** (run 2026-06-02 from the US client
against the relay `TARGET=63.185.9.233:41748`). It runs dimensions 1, 2, 4, 5 for both
protocols and writes `{connection,latency,scale,throughput}/` CSVs. The CSV parameters
confirm this orchestrator (scale `duration=20`, throughput `n=50 duration=10`, latency
`n=10`). It does **not** run multistream — dimension 3 was the separate raw_mini run (see
Dimension 3 provenance above), and the `multistream/` and `lan/` subfolders were added into
`compare_run/` separately.

Earlier (2026-06-01) exploration used `bench/run_full.sh` (e.g.
`nohup env TARGET=63.185.9.233:41748 INTER_TEST_DELAY=30 ./bench/run_full.sh ...`) and
`run_all.sh` — these appear in the shell histories, used `duration=30`, and wrote
`run_<timestamp>/` dirs. `run_full.sh` also has a multistream step but it is **broken** (it
points `multiplex_bench` at the relay → ALPN/framing failure, all-error logs), so it was not
used for dimension 3. Charts: `python3 bench/chart_compare.py bench_results/compare_run`.

## One-line summary table

| Dim | Mode | Key params | Metric | Winner |
|---|---|---|---|---|
| 1 Connection | `dial` | n=10..500 | dial_ms | **QUIC ~2×** |
| 2 Latency | `latency` | sizes 64B..64KB, n=10 | RTT p50/p95 | tie ≤4KB / **QUIC** large |
| 3 Multistream | `multiplex_bench` | N=4..32, size=1KB | total elapsed | **QUIC 20–34%** |
| 4 Scale | `scale` | n=10..1000, rate=1, 20s | delivery, RTT | tie (both 100%); TCP lower RTT @1000 |
| 5 Throughput | `throughput` | rates 1..100, n=50, 10s | sustained/delivery | tie |
