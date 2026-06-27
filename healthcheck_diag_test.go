package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestHealthCheckWithRealQUIC tests the full PING/PONG health check loop
// using real QUIC connections and verifies no false-positive disconnects.
func TestHealthCheckWithRealQUIC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long-running test in short mode")
	}

	// Create relay ConnectionManager
	relayCM := NewConnectionManager(0, "diagroom", false, "", true, "DiagRelay")

	// Start relay server
	relayServer, err := NewServer("127.0.0.1:0", nil, nil, relayCM)
	if err != nil {
		t.Fatalf("failed to start relay server: %v", err)
	}
	defer relayServer.Close()
	relayCM.localPort = relayServer.Port()
	relayAddr := fmt.Sprintf("127.0.0.1:%d", relayServer.Port())

	// Create non-relay ConnectionManager
	nonRelayCM := NewConnectionManager(0, "diagroom", false, "", false, "DiagClient")
	nonRelayCM.MarkRelay(relayAddr)

	// Connect non-relay to relay
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mc, err := nonRelayCM.getOrCreate(ctx, relayAddr)
	cancel()
	if err != nil {
		t.Fatalf("failed to connect non-relay to relay: %v", err)
	}
	_ = mc

	// Wait for connection to be established and JOIN processed
	time.Sleep(500 * time.Millisecond)

	// Verify connections exist
	relayPeers := relayCM.ListConnected()
	nonRelayPeers := nonRelayCM.ListConnected()
	t.Logf("Relay peers: %v", relayPeers)
	t.Logf("Non-relay peers: %v", nonRelayPeers)

	if len(relayPeers) == 0 {
		t.Fatal("relay has no connected peers")
	}
	if len(nonRelayPeers) == 0 {
		t.Fatal("non-relay has no connected peers")
	}

	initialRelayPeerCount := len(relayPeers)
	maxDisconnects := 0

	// Now let the healthCheck run for a while
	// Send a message every 3 seconds to simulate chat activity
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for i := 0; i < 10; i++ {
			select {
			case <-done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err := nonRelayCM.Send(ctx, relayAddr, fmt.Sprintf("test message %d", i))
				cancel()
				if err != nil {
					t.Logf("Send error at iteration %d: %v", i, err)
					// Try to reconnect
					ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
					_, err2 := nonRelayCM.getOrCreate(ctx2, relayAddr)
					cancel2()
					if err2 != nil {
						t.Logf("Reconnect failed: %v", err2)
					}
				}
			}
		}
	}()

	// Monitor for 35 seconds (7 healthCheck ticks)
	monitorDuration := 35 * time.Second
	t.Logf("Monitoring health check for %v...", monitorDuration)

	monitorTicker := time.NewTicker(5 * time.Second)
	defer monitorTicker.Stop()
	timeout := time.After(monitorDuration)

	checkCount := 0
	for {
		select {
		case <-timeout:
			close(done)
			goto done
		case <-monitorTicker.C:
			checkCount++
			relayPeers := relayCM.ListConnected()
			nonRelayPeers := nonRelayCM.ListConnected()

			relayDisconnects := initialRelayPeerCount - len(relayPeers)
			if relayDisconnects > maxDisconnects {
				maxDisconnects = relayDisconnects
			}

			t.Logf("[t=%ds] Relay: %d peers %v | Non-relay: %d peers %v | Lost: %d",
				checkCount*5, len(relayPeers), relayPeers,
				len(nonRelayPeers), nonRelayPeers,
				relayDisconnects)

			if len(relayPeers) == 0 || len(nonRelayPeers) == 0 {
				t.Logf("WARNING: One side lost all connections at t=%ds", checkCount*5)
			}
		}
	}

done:
	// Final check
	relayPeers = relayCM.ListConnected()
	nonRelayPeers = nonRelayCM.ListConnected()

	// Check pendingPings state
	relayCM.pendingPingsMu.Lock()
	relayPending := make(map[string]time.Time)
	for k, v := range relayCM.pendingPings {
		relayPending[k] = v
	}
	relayCM.pendingPingsMu.Unlock()

	nonRelayCM.pendingPingsMu.Lock()
	nonRelayPending := make(map[string]time.Time)
	for k, v := range nonRelayCM.pendingPings {
		nonRelayPending[k] = v
	}
	nonRelayCM.pendingPingsMu.Unlock()

	t.Logf("FINAL: Relay peers: %d %v | Non-relay: %d %v | Max disconnects: %d",
		len(relayPeers), relayPeers, len(nonRelayPeers), nonRelayPeers,
		maxDisconnects)
	t.Logf("Relay pendingPings: %s", formatPendingPings(relayPending))
	t.Logf("Non-relay pendingPings: %s", formatPendingPings(nonRelayPending))

	if maxDisconnects > 0 {
		t.Errorf("Detected %d unexpected disconnection(s)", maxDisconnects)
	}

	if len(relayPeers) == 0 || len(nonRelayPeers) == 0 {
		t.Error("One or both sides lost all connections - health check false positive")
	}
}

func formatPendingPings(pings map[string]time.Time) string {
	if len(pings) == 0 {
		return "(none)"
	}
	var parts []string
	for addr, t := range pings {
		age := time.Since(t)
		parts = append(parts, fmt.Sprintf("%s(%v ago)", addr, age.Round(time.Second)))
	}
	return strings.Join(parts, ", ")
}
