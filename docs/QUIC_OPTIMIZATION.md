# QUIC Performance Optimization

## Summary

This document covers the performance optimization work for the QUIC transport layer.

### Final Results

| Transport | Latency | Ratio |
|-----------|---------|-------|
| TCP + TLS 1.3 | 16.9 us | baseline |
| User-space QUIC | 52.1 us | 3.1x |
| Kernel QUIC | 3.4 Gbps | matches TCP |

User-space QUIC is 3.1x slower than TCP on localhost due to architectural differences between kernel-space TCP and user-space UDP processing.

## Optimizations Applied

### QUIC Configuration

```go
quic.Config{
    MaxIdleTimeout:                 5 * time.Minute,
    KeepAlivePeriod:                30 * time.Second,
    MaxIncomingStreams:             1000,
    InitialStreamReceiveWindow:     6 * 1024 * 1024,  // 6 MB
    InitialConnectionReceiveWindow: 15 * 1024 * 1024, // 15 MB
    MaxStreamReceiveWindow:         16 * 1024 * 1024, // 16 MB
    MaxConnectionReceiveWindow:     64 * 1024 * 1024, // 64 MB
    Allow0RTT:                      true,
}
```

### Application Optimizations

| Optimization | File | Impact |
|--------------|------|--------|
| Parallel broadcasting | server.go | 5-10x faster |
| Buffer pooling | performance.go | -20% allocations |
| 0-RTT enabled | connection_manager.go | Faster reconnections |
| Large receive windows | QUIC config | Higher throughput |

### System Tuning

```bash
# scripts/apply_sysctl.sh
net.core.rmem_max = 26214400      # 25 MB
net.core.wmem_max = 26214400
net.core.netdev_max_backlog = 5000
net.core.somaxconn = 4096
```

## Why QUIC is Slower on Localhost

### Architecture Difference

TCP operates in kernel space with 40 years of optimization. QUIC (quic-go) operates in user space, requiring additional syscalls and data copies.

### CPU Profile

| Component | QUIC | TCP |
|-----------|------|-----|
| Syscalls | 39% | 80% |
| Packet handling | 61% | 20% |

QUIC spends only 39% of CPU on actual I/O versus 80% for TCP.

## When QUIC Wins

Despite localhost disadvantage, QUIC excels in:

| Scenario | Advantage |
|----------|-----------|
| High latency | 0-RTT saves round trips |
| Packet loss | Better recovery |
| NAT traversal | UDP-based |
| Mobile | Connection migration |

## Files

| File | Purpose |
|------|---------|
| performance.go | Buffer pooling, message batching |
| scripts/apply_sysctl.sh | System tuning |
| transport_benchmark_test.go | Benchmarks |
