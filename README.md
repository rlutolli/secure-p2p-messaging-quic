# Secure P2P Messaging with QUIC

## Overview

A decentralized peer-to-peer messaging system built in Go that uses the QUIC transport protocol for secure, low-latency communication. The application enables real-time text messaging between peers on a Local Area Network (LAN) with automatic peer discovery, room-based grouping, and end-to-end TLS 1.3 encryption.

## Features

- Secure Communication: TLS 1.3 encryption via QUIC protocol
- Automatic Discovery: UDP broadcast-based LAN peer discovery
- Room-Based Chat: Logical grouping of peers into rooms
- Mesh Networking: Bidirectional message relay between peers
- Connection Pooling: Persistent QUIC connections for efficiency
- Dual Transport: Support for both QUIC and TCP+TLS

## Quick Start

```bash
# Build
go build -o p2p-messenger .

# Run
./p2p-messenger

# Run with TCP instead of QUIC
./p2p-messenger --use-tcp

# Run with debug logging
./p2p-messenger --debug
```

## Architecture

### File Structure

| File | Purpose |
|------|---------|
| `main.go` | Application entry point, CLI interface |
| `server.go` | QUIC/TCP server, room management, message broadcasting |
| `connection_manager.go` | Outgoing connection pooling, message deduplication |
| `discovery.go` | UDP-based LAN peer discovery |
| `security.go` | TLS 1.3 certificate generation |
| `transport.go` | Transport abstraction for QUIC/TCP |
| `performance.go` | Buffer pooling and optimization utilities |

### Message Protocol

| Prefix | Format | Description |
|--------|--------|-------------|
| `JOIN:` | `JOIN:<room>` | Peer requests to join a room |
| `FROM:` | `FROM:<alias>\|<message>` | User message with sender ID |
| `SYSTEM:` | `SYSTEM:<message>` | Join/leave notifications |

### Discovery Protocol

- Port: UDP 19999
- Announce Interval: 3 seconds
- Peer Expiry: 15 seconds

## Commands

| Command | Description |
|---------|-------------|
| `<message>` | Send message to room |
| `/help` | Show available commands |
| `/peers` | List connected peers |
| `/connect <addr> [msg]` | Connect to specific peer |
| `/room` | Show room info |
| `exit` | Exit application |

## Performance

See `docs/QUIC_OPTIMIZATION.md` for detailed benchmarks.

| Transport | Latency |
|-----------|---------|
| TCP + TLS 1.3 | 16.9 us |
| QUIC | 52.1 us |

## Testing

```bash
# Run benchmarks
go test -bench=. -benchmem

# Run unit tests
go test -v
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/quic-go/quic-go` | QUIC protocol implementation |
| `golang.org/x/crypto` | Cryptographic primitives |
| `golang.org/x/sys` | System calls for socket options |
