# Performance Optimization Guide

## Current Performance Characteristics

The application uses QUIC (HTTP/3 transport) which provides:
- **Low latency**: 0-RTT connection resumption
- **Multiplexing**: Multiple streams per connection
- **Built-in encryption**: TLS 1.3
- **Connection migration**: Handles network changes

## Identified Performance Bottlenecks

### 1. **Synchronous Broadcasting** (High Priority)
**Location:** `server.go:broadcastToRoom()`

**Issue:** Messages are broadcast sequentially, blocking on each Write() call.

**Impact:** With 10 peers, if each write takes 1ms, total broadcast time = 10ms

**Solution:**
```go
// Parallel broadcast with goroutines
func (s *Server) broadcastToRoom(room *Room, senderAddr, message string) {
    room.peersMu.RLock()
    peers := make([]*Peer, 0, len(room.peers))
    for _, p := range room.peers {
        if p.addr != senderAddr {
            peers = append(peers, p)
        }
    }
    room.peersMu.RUnlock()

    formatted := fmt.Sprintf("FROM:%s|%s\n", senderAddr, message)
    msgBytes := []byte(formatted)
    
    // Parallel broadcast
    var wg sync.WaitGroup
    for _, peer := range peers {
        wg.Add(1)
        go func(p *Peer) {
            defer wg.Done()
            if _, err := p.stream.Write(msgBytes); err != nil {
                log.Printf("[Server] Broadcast error to %s: %v", p.addr, err)
            }
        }(peer)
    }
    wg.Wait()
}
```

**Expected improvement:** 5-10x faster broadcasts with many peers

---

### 2. **Single Stream Per Connection** (High Priority)
**Location:** `server.go:handleConnection()`, `connection_manager.go`

**Issue:** Only one stream per QUIC connection, limiting throughput.

**Solution:** Use multiple streams for parallel message handling
```go
// Allow multiple streams per connection
type Peer struct {
    addr    string
    conn    *quic.Conn
    streams map[quic.StreamID]*quic.Stream
    streamMu sync.RWMutex
    room    *Room
}

// Use separate streams for sending/receiving
// Or use unidirectional streams for broadcasts
```

**Expected improvement:** 2-3x throughput increase

---

### 3. **Lock Contention in Room Management** (Medium Priority)
**Location:** `server.go:joinRoom()`, `broadcastToRoom()`

**Issue:** Holding room lock while broadcasting blocks new joins.

**Solution:** Copy peer list before releasing lock
```go
// Already implemented correctly, but can optimize further:
// Use sync.Map for rooms if room count is high
// Use lock-free data structures for read-heavy operations
```

---

### 4. **Message Formatting Overhead** (Low Priority)
**Location:** `server.go:broadcastToRoom()`, `connection_manager.go:Send()`

**Issue:** String formatting happens for every message.

**Solution:** Pre-format or use byte buffers
```go
// Use bytes.Buffer or pre-allocated buffers
var msgPool = sync.Pool{
    New: func() interface{} {
        return &bytes.Buffer{}
    },
}

func (s *Server) broadcastToRoom(...) {
    buf := msgPool.Get().(*bytes.Buffer)
    defer msgPool.Put(buf)
    buf.Reset()
    
    buf.WriteString("FROM:")
    buf.WriteString(senderAddr)
    buf.WriteString("|")
    buf.WriteString(message)
    buf.WriteByte('\n')
    
    msgBytes := buf.Bytes()
    // ... broadcast
}
```

**Expected improvement:** 10-20% reduction in CPU usage

---

### 5. **QUIC Configuration Tuning** (High Priority)
**Location:** `server.go:NewServer()`, `connection_manager.go:GetOrCreate()`

**Current:** Basic timeout settings

**Optimized Configuration:**
```go
&quic.Config{
    MaxIdleTimeout:        60 * time.Second,  // Increase for persistent connections
    KeepAlivePeriod:       15 * time.Second, // Balance between keepalive and overhead
    MaxIncomingStreams:    100,               // Allow more concurrent streams
    MaxIncomingUniStreams: 100,
    InitialStreamReceiveWindow:     1 << 20,  // 1 MB
    InitialConnectionReceiveWindow: 1 << 22,  // 4 MB
    MaxStreamReceiveWindow:         1 << 20,
    MaxConnectionReceiveWindow:     1 << 22,
    AllowConnectionWindowIncrease:  func(conn *quic.Conn, delta uint64) bool { return true },
    // Enable 0-RTT
    Allow0RTT: true,
    // Tune congestion control
    DisablePathMTUDiscovery: false,
}
```

**Expected improvement:** 20-30% better throughput, lower latency

---

### 6. **Connection Pooling** (Medium Priority)
**Location:** `connection_manager.go`

**Issue:** New connection for each peer discovery.

**Solution:** Reuse connections, implement connection health checks
```go
type ManagedConnection struct {
    conn      *quic.Conn
    stream    *quic.Stream
    peerAddr  string
    createdAt time.Time
    lastUsed  time.Time  // Track usage
    mu        sync.Mutex
    healthy   bool       // Connection health flag
}

// Add health check goroutine
func (cm *ConnectionManager) healthCheck() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        cm.mu.RLock()
        for addr, mc := range cm.connections {
            // Check if connection is still alive
            if time.Since(mc.lastUsed) > 60*time.Second {
                // Connection idle, but keep it
            }
        }
        cm.mu.RUnlock()
    }
}
```

---

### 7. **mDNS Discovery Optimization** (Low Priority)
**Location:** `discovery.go`

**Issue:** Discovery timeout is 3 seconds, blocking sends.

**Solution:** 
- Cache discovered peers
- Background discovery refresh
- Use shorter timeout with retries

```go
type DiscoveryService struct {
    // ... existing fields
    peerCache map[string]time.Time  // addr -> last seen
    cacheMu   sync.RWMutex
    refreshTicker *time.Ticker
}

// Background refresh every 5 seconds
func (ds *DiscoveryService) startBackgroundRefresh() {
    ds.refreshTicker = time.NewTicker(5 * time.Second)
    go func() {
        for range ds.refreshTicker.C {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            peers, _ := ds.LookupPeers(ctx)
            cancel()
            // Update cache
        }
    }()
}
```

---

### 8. **Batch Message Sending** (Medium Priority)
**Location:** `connection_manager.go:Send()`

**Issue:** Each message is a separate write operation.

**Solution:** Implement message batching for high-frequency sends
```go
type MessageBatch struct {
    messages []string
    mu       sync.Mutex
    timer    *time.Timer
}

// Batch messages within 10ms window
func (cm *ConnectionManager) SendBatch(ctx context.Context, peerAddr string, message string) error {
    // Add to batch, flush after timeout or batch size limit
}
```

---

### 9. **Memory Pooling** (Low Priority)
**Location:** All message handling code

**Issue:** Frequent allocations for message buffers.

**Solution:** Use sync.Pool for buffers
```go
var bufferPool = sync.Pool{
    New: func() interface{} {
        return make([]byte, 0, 4096)
    },
}

func getBuffer() []byte {
    return bufferPool.Get().([]byte)
}

func putBuffer(buf []byte) {
    buf = buf[:0]
    bufferPool.Put(buf)
}
```

---

### 10. **Profiling and Metrics** (Essential)
**Add performance monitoring:**

```go
// Add metrics collection
type Metrics struct {
    MessagesSent     int64
    MessagesReceived int64
    BroadcastLatency  time.Duration
    ConnectionCount  int64
    mu               sync.RWMutex
}

// Use pprof for profiling
import _ "net/http/pprof"

func init() {
    go func() {
        log.Println(http.ListenAndServe("localhost:6060", nil))
    }()
}
```

---

## Implementation Priority

### Phase 1: Quick Wins (1-2 days)
1. ✅ Parallel broadcasting (#1)
2. ✅ QUIC configuration tuning (#5)
3. ✅ Message buffer pooling (#9)

**Expected improvement:** 3-5x performance boost

### Phase 2: Architecture (3-5 days)
4. ✅ Multiple streams per connection (#2)
5. ✅ Connection health checks (#6)
6. ✅ mDNS caching (#7)

**Expected improvement:** Additional 2-3x improvement

### Phase 3: Advanced (1 week)
7. ✅ Batch message sending (#8)
8. ✅ Advanced profiling (#10)
9. ✅ Lock-free optimizations (#3)

**Expected improvement:** Additional 1.5-2x improvement

---

## Benchmarking

Create a benchmark script:

```go
// benchmark_test.go
func BenchmarkBroadcast(b *testing.B) {
    // Setup server with N peers
    // Measure broadcast time
}

func BenchmarkConnectionCreation(b *testing.B) {
    // Measure connection setup time
}
```

Run with:
```bash
go test -bench=. -benchmem -cpuprofile=cpu.prof -memprofile=mem.prof
go tool pprof cpu.prof
```

---

## Target Performance Metrics

- **Message Latency:** < 10ms (local network)
- **Broadcast Time:** < 50ms for 50 peers
- **Connection Setup:** < 100ms
- **Throughput:** > 10,000 messages/second
- **Memory:** < 50MB per 100 peers
- **CPU:** < 10% idle load with 100 peers

---

## Next Steps

1. Implement Phase 1 optimizations
2. Run benchmarks to establish baseline
3. Profile with pprof to find actual bottlenecks
4. Iterate based on profiling results
5. Add metrics collection for production monitoring

