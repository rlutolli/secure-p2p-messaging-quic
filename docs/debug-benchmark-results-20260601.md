# Debug Brief: Benchmark Run 2026-06-01 — Scale and Throughput Failures

## 1. What Was Run

**Command executed (US client, EC2 Virginia):**
```bash
nohup env TARGET=63.185.9.233:41748 INTER_TEST_DELAY=30 ./bench/run_full.sh > bench.log 2>&1 &
```

**Relay (EU, EC2 Frankfurt):**
```bash
./p2p-messenger --relay --no-upnp --disable-gso --disable-ecn --relay-port 41748
```

**Environment:**
- EU Relay: `63.185.9.233:41748` (t3.large, 8GB RAM)
- US Client: `52.7.165.55` (t3.large, 8GB RAM)
- Cross-region WAN path (AWS inter-region)
- UDP buffers tuned: `net.core.rmem_max=8388608`, `net.core.wmem_max=8388608`
- GSO/ECN disabled (the WAN fix is active)

## 2. Results Summary

```
  Output directory: ./bench_results/run_20260601_122547
  Total tests:      42
  Failures:         2
  Degraded:         12
  RESULT: FAIL
```

### 2.1 What Passed Flawlessly

| Dimension | Details |
|---|---|
| **Connection (dial)** | ALL OK. TCP and QUIC, n=10 to n=500. Zero errors. Dialing works even at high scale. |
| **Latency** | ALL OK. Minor 90% delivery for small payloads (64B), 100% for 16KB+. This is expected (small messages can be dropped under load). |
| **Multistream** | ALL OK. TCP and QUIC raw transport multiplexing works. |
| **Scale n=50, n=200** | ALL OK. 100% delivery for both TCP and QUIC. |

### 2.2 What Failed

#### Scale Tests — The Relay Broadcast Bottleneck

| Test | Status | Detail |
|---|---|---|
| scale tcp n=500 | **FAIL** | errors=2 |
| scale tcp n=1000 | **FAIL** | errors=467 |
| scale quic n=500 | **DEGRADED** | delivery=11881/14997 (79.2%) |
| scale quic n=1000 | **DEGRADED** | delivery=0/15603 (0.0%) |

**Pattern:** The relay handles **200 peers perfectly** but breaks at **500+ peers**. At n=1000, QUIC fails completely (0% delivery) while TCP accumulates 467 real errors.

**Root cause hypothesis:** `broadcastToRoom()` in `server.go` sends messages to ALL peers in the room sequentially (with a WaitGroup). At n=500+, the relay is overwhelmed trying to write to 500+ connections for every single message. Each peer sends 1 msg/s, so at n=500 the relay must perform **500 writes × 500 peers = 250,000 writes per second**. The goroutine and write volume exceeds what a single Go process can sustain over WAN.

#### Throughput Tests — Complete Failure at All Rates

| Test | Status | Detail |
|---|---|---|
| throughput tcp rate=1 | **DEGRADED** | delivery=0/500 (0.0%) |
| throughput tcp rate=5 | **DEGRADED** | delivery=0/2501 (0.0%) |
| throughput tcp rate=10 | **DEGRADED** | delivery=0/5000 (0.0%) |
| throughput tcp rate=50 | **DEGRADED** | delivery=0/25000 (0.0%) |
| throughput tcp rate=100 | **DEGRADED** | delivery=0/50000 (0.0%) |
| throughput quic rate=1 | **DEGRADED** | delivery=0/500 (0.0%) |
| throughput quic rate=5 | **DEGRADED** | delivery=0/2500 (0.0%) |
| throughput quic rate=10 | **DEGRADED** | delivery=0/5000 (0.0%) |
| throughput quic rate=50 | **DEGRADED** | delivery=0/50000 (0.0%) |
| throughput quic rate=100 | **DEGRADED** | delivery=0/50000 (0.0%) |

**Pattern:** Throughput mode uses **n=50 peers**, which works fine in scale mode (100% delivery). But in throughput mode, **delivery is 0% at ALL rates** for both TCP and QUIC.

**Critical observation:** Scale mode at n=50, rate=1, duration=30s = 1500/1500 delivery (100%). Throughput mode at rate=1, duration=10s = 0/500 delivery (0%). Same peer count, same rate, different outcome.

**Root cause hypothesis:** This is likely a **mode-specific bug in `loadtest/main.go`**, not a relay capacity issue. The throughput mode may:
1. Use a different message format that the relay doesn't broadcast properly
2. Have a race condition in the receive tracker (`globalTracker`) specific to throughput aggregation
3. Exit too quickly (duration=10s vs scale's 30s) before replies arrive
4. Use `-rates` flag parsing incorrectly, causing peers to not actually send messages

## 3. Key Code Locations to Investigate

### 3.1 Relay Broadcast Bottleneck (`server.go`)

File: `server.go`, lines 303-360, function `broadcastToRoom()`

Current behavior:
- Iterates ALL peers in room (excluding sender)
- Launches a goroutine per peer for each write
- Uses `sync.WaitGroup` to wait for all writes
- At n=500 with rate=1 msg/s/peer: ~250,000 goroutine launches per second

**What to try:**
- Add a worker pool to limit concurrent writes (e.g., 64 workers)
- Batch broadcasts instead of per-message goroutines
- Measure `broadcastToRoom()` latency — if it exceeds 1s, messages queue up and peers timeout

### 3.2 Throughput Mode Bug (`loadtest/main.go`)

File: `loadtest/main.go`, function `runThroughputMode()` and `runPeer()`

**Known differences from scale mode:**
- Scale: `-mode scale -n 50 -rate 1 -duration 30`
- Throughput: `-mode throughput -n 50 -rates "1" -duration 10`

**Questions to answer:**
1. Does `runThroughputMode()` call `runPeer()` with the correct rate? Check `localCfg.rate = rate` assignment.
2. Does throughput mode wait for replies? Scale mode has a 15s post-deadline sleep. Does throughput mode also have this?
3. Is there a bug in the throughput CSV aggregation? `aggregateThroughputResults` uses `msgsSent` and `msgsRecv` — verify these counters are incremented correctly.
4. Check if throughput mode's `globalTracker` is shared properly across rate iterations.

### 3.3 Connection Manager (`connection_manager.go`)

Check:
- `MaxIdleTimeout` and `KeepAlivePeriod` — are connections being closed mid-test?
- `removeConnection()` — is the relay closing peers under memory pressure?
- `healthCheck()` — at n=1000, does the PING/PONG traffic itself overwhelm the relay?

## 4. Immediate Diagnostic Steps

### Step 1: Verify throughput mode locally
```bash
# Terminal 1: start relay
./p2p-messenger --relay --relay-port 33333

# Terminal 2: run throughput mode with verbose output
./loadtest-bin -mode throughput -proto quic -n 2 -rates "1" -duration 10 -target 127.0.0.1:33333 -room test -csv=false
```

If this shows 0% delivery locally, it's a `loadtest` bug, not a WAN issue.

### Step 2: Check raw logs
```bash
# On US client
cat ./bench_results/run_20260601_122547/scale/scale_tcp_1000.raw.log
cat ./bench_results/run_20260601_122547/throughput/throughput_tcp_1.raw.log
```

Look for:
- Are peers actually sending messages? (check for "msgs_sent" in raw output)
- Are peers receiving anything at all?
- What are the 467 TCP errors at n=1000? (connection reset? timeout?)

### Step 3: Profile relay during n=500 test
```bash
# On EU relay, while test runs:
curl http://localhost:6060/debug/pprof/goroutine?debug=1
```
(If pprof is not enabled, add `import _ "net/http/pprof"` to `main.go` temporarily)

Check if goroutine count explodes during scale test.

## 5. Hypotheses Ranked by Likelihood

| Rank | Hypothesis | Evidence | Test |
|---|---|---|---|
| 1 | **Throughput mode has a receive-tracking or wait-time bug** | n=50 works in scale mode (100%) but fails in throughput mode (0%) across ALL rates | Run throughput locally with `-csv=false` and verbose logging |
| 2 | **Relay broadcast serializes too slowly at n≥500** | Dial works fine; only message delivery fails; quic n=1000 shows 0% delivery | Profile `broadcastToRoom()` latency; add worker pool |
| 3 | **HealthCheck PING/PONG floods the relay at high peer counts** | 500 peers × PING every 5s = 100 PINGs/s inbound, plus 100 PONGs/s outbound = 200 msg/s overhead | Reduce healthCheck interval or disable it during benchmarks |
| 4 | **TCP accept backlog overflows at n=1000** | TCP dial fails with "connection refused" or "reset by peer" | Check `netstat -s \| grep overflow` on relay during test |
| 5 | **QUIC stream limits** | QUIC n=1000 shows 0% delivery (complete failure) | Check if `MaxIncomingStreams` in `server.go` is set too low |

## 6. Context: What Has Already Been Fixed

Do NOT waste time on these — they are already resolved:
- ✅ GSO/ECN WAN bug (default-disabled via `--disable-gso` / `--disable-ecn`)
- ✅ Staggered dialing (`-dial-stagger` auto 5ms for n≥500)
- ✅ Retry logic for transient dial errors
- ✅ Benign error filtering (Application error 0x0, EOF, etc.)
- ✅ Relay health check in `run_full.sh`
- ✅ `--relay-port` fixed port binding
- ✅ TOCTOU race in `healthCheck()`
- ✅ Concurrent writes in `broadcastToRoom()`

## 7. Files Where Changes Are Likely Needed

| File | What to Change |
|---|---|
| `server.go` | Optimize `broadcastToRoom()` — add worker pool, measure latency |
| `loadtest/main.go` | Debug `runThroughputMode()` — why 0% delivery at n=50? |
| `connection_manager.go` | Consider reducing PING frequency or disabling healthCheck for benchmarks |
| `bench/run_full.sh` | Consider adding `--skip-throughput` or reducing throughput `n` until fixed |

## 8. Reproduction Command

To reproduce the exact same results:
```bash
# Relay (EU)
./p2p-messenger --relay --no-upnp --disable-gso --disable-ecn --relay-port 41748

# Client (US)
env TARGET=63.185.9.233:41748 INTER_TEST_DELAY=30 ./bench/run_full.sh
```

---

**Next AI task:** Determine WHY throughput mode shows 0% delivery at n=50 (which works in scale mode), and WHY scale mode degrades at n≥500. Fix the root cause and verify with a fresh benchmark run.

---

## 9. RESOLUTION (2026-06-02)

### TL;DR
Both symptoms had **one** root cause in the relay's `broadcastToRoom()`, not two
separate bugs. The throughput "bug" was a red herring: throughput mode shares the
exact same `runPeer()` code as scale mode and delivers 100% in isolation. It only
showed 0% on the full WAN run because it runs **last**, right after `scale n=1000`
wrecked the relay — and the relay never recovered (its TCP listener still answered
the health probe, so the suite kept going against a dead relay).

### Root cause
`broadcastToRoom()` ran on the **sender's per-peer reader goroutine** and ended in
`wg.Wait()`, after spawning one goroutine per peer per message. Two consequences at
n≥500:

1. **Reader starvation → false health-check disconnects.** While a broadcast was in
   flight, the reader could not process incoming `PONG`s. `healthCheck()` PINGs every
   peer every 5s and kills any peer silent for 10s, so live peers were torn down en
   masse, cascading to total delivery failure.
2. **Goroutine explosion + no slow-peer isolation.** ~250k goroutine launches/sec at
   n=500, and a single slow/dead peer's `Write` stalled the whole fan-out.

### Fix
Implemented the standard relay pattern: a **bounded per-connection outbound queue
drained by one dedicated writer goroutine** (`ManagedConnection.out` +
`ConnectionManager.writeLoop`).

- `broadcastToRoom()` now does a **non-blocking enqueue** per peer (O(1), never blocks
  the reader). A peer whose queue is full drops its own message instead of stalling
  everyone.
- Per-connection writes are serialised by the writer goroutine, which also removes the
  previously-flagged unsafe **concurrent QUIC stream writes** (PING vs broadcast).
- Files changed: `server.go` (`broadcastToRoom`), `connection_manager.go`
  (`ManagedConnection`, `newManagedConnection`, `enqueue`, `writeLoop`, `Enqueue`,
  `signalClosed`; wired into `getOrCreate`/`RegisterIncoming`/`removeConnection`/`Close`),
  `performance.go` (`OutboundQueueSize = 1024`), `relay_test.go` (use constructor).

### Verified on the real EU↔US WAN path (relay 63.185.9.233:41748)

| Test | Before | After |
|---|---|---|
| scale quic n=500 | 79.2% (11881/14997) | **100% (10000/10000)**, 0 err, 0 disc |
| scale quic n=1000 | **0% (0/15603)** | **100% (20001/20001)**, 0 err, 0 disc |
| scale tcp n=1000 | 467 errors | **100% (20001/20001)**, 0 errors |
| throughput quic rate=1..100 | 0% at all rates | **100% at all rates**, 0 err, 0 disc |
| throughput tcp rate=1..100 | 0% at all rates | **100% at all rates**, 0 err, 0 disc |

Relay survived the entire run (200k+ messages, up to 999-way fan-out) at **48 MB RSS**
with no errors/panics. Full `go test ./...` and `go test -race` pass.

**Note for the bench harness:** at n=1000 the single relay's broadcast RTT tail grows
(p50 ~430ms, max ~2.4s) under ~1M relayed writes/sec — expected for one Go process over
WAN. Delivery is complete and lossless; only latency rises. No `--skip-throughput`
workaround is needed.
