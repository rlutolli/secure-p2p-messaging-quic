# Testing Instructions for Secure P2P Messenger

## Prerequisites

1. **Build the application:**
   ```bash
   go build -o p2p-messenger .
   ```

2. **Increase UDP buffer sizes (recommended for better performance):**
   ```bash
   # Linux
   sudo sysctl -w net.core.rmem_max=8388608
   sudo sysctl -w net.core.wmem_max=8388608
   
   # Or make it permanent by adding to /etc/sysctl.conf:
   # net.core.rmem_max = 8388608
   # net.core.wmem_max = 8388608
   ```

## Basic Testing (Single Machine)

### Test 1: Two Peers in Same Room

**Terminal 1:**
```bash
./p2p-messenger
# Enter room name: test-room
# Wait for startup, then type:
send Hello from peer 1!
```

**Terminal 2:**
```bash
./p2p-messenger
# Enter room name: test-room
# Wait a moment, then type:
peers
# You should see the first peer's address
send Hello from peer 2!
```

**Expected behavior:**
- Both peers should discover each other
- Messages should be received and displayed
- Format: `[sender-address]: message`

### Test 2: Multiple Peers

Open 3-4 terminals, all join the same room, and send messages. All should receive broadcasts.

### Test 3: Private Mode

**Terminal 1:**
```bash
./p2p-messenger
# Enter room name: private
```

**Terminal 2:**
```bash
./p2p-messenger
# Enter room name: test-room
peers
# Should not find the private peer
```

### Test 4: Direct Connection

**Terminal 1:**
```bash
./p2p-messenger
# Note the port number shown (e.g., 48692)
```

**Terminal 2:**
```bash
./p2p-messenger
# Enter room name: test-room
connect 127.0.0.1:48692 Hello direct!
```

## Network Testing (Multiple Machines)

1. **Find your local IP:**
   ```bash
   # Linux/Mac
   ip addr show | grep "inet " | grep -v 127.0.0.1
   # or
   hostname -I
   ```

2. **On Machine 1:**
   ```bash
   ./p2p-messenger
   # Enter room name: network-test
   # Note the port (e.g., 48692)
   ```

3. **On Machine 2 (same network):**
   ```bash
   ./p2p-messenger
   # Enter room name: network-test
   peers
   # Should discover Machine 1
   send Test message!
   ```

## Testing Commands

- `send <message>` - Send message to all discovered peers in room
- `peers` - List all discovered peers
- `connect <ip:port> [message]` - Connect directly to a peer
- `quit` or `exit` - Exit the application

## Troubleshooting

1. **No peers found:**
   - Ensure both peers are on the same network
   - Check firewall settings (UDP port must be open)
   - Try `connect` with explicit IP:port

2. **Connection errors:**
   - Verify the port number is correct
   - Check if firewall is blocking UDP
   - Ensure both machines can reach each other

3. **mDNS not working:**
   - Some networks block mDNS (multicast)
   - Use `connect` command as fallback
   - Check with: `avahi-browse -a` (Linux) or `dns-sd -B _p2pmsg._udp` (Mac)

## Performance Testing

For performance benchmarks, you can:

1. **Measure message latency:**
   - Send timestamped messages
   - Calculate round-trip time

2. **Test with many peers:**
   - Start 10+ instances
   - Send messages and measure broadcast time

3. **Monitor resource usage:**
   ```bash
   # Monitor CPU and memory
   top -p $(pgrep p2p-messenger)
   ```

