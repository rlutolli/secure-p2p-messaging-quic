package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// With transport racing enabled, a client should connect to a dual-stack relay
// (which listens on both QUIC and TCP) — whichever handshake wins the race.
func TestRaceDialConnectsToDualStackRelay(t *testing.T) {
	relayCM := NewConnectionManager(0, "raceroom", false, "", true, "RaceRelay")
	defer relayCM.Close()

	srv, err := NewServer("127.0.0.1:0", nil, nil, relayCM)
	if err != nil {
		t.Fatalf("failed to start relay server: %v", err)
	}
	defer srv.Close()
	relayCM.localPort = srv.Port()
	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port())

	cm := NewConnectionManager(0, "raceroom", false, "", false, "RaceClient")
	cm.raceTransports = true
	defer cm.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mc, err := cm.GetOrCreate(ctx, addr)
	if err != nil {
		t.Fatalf("race dial failed: %v", err)
	}
	if mc == nil {
		t.Fatal("race dial returned nil connection")
	}
	if got := len(cm.ListConnected()); got != 1 {
		t.Errorf("expected 1 registered connection, got %d", got)
	}
}
