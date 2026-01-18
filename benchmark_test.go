package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// noOpHandler is a message handler that does nothing, used for benchmarks
func noOpHandler(from, room, msg string) {}
func noOpSysHandler(msg string)          {}

// setupBenchmarkEnvironment sets up a server and returns its address and a cleanup function
func setupBenchmarkEnvironment(useTCP bool) (*Server, string, func(), error) {
	// Find a free port by letting the OS choose (0)
	server, err := NewServer("127.0.0.1:0", noOpHandler, noOpSysHandler, nil)
	if err != nil {
		return nil, "", nil, err
	}

	// Wait a bit for server to start
	time.Sleep(10 * time.Millisecond)

	serverPort := server.Port()
	addr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	cleanup := func() {
		server.Close()
	}

	return server, addr, cleanup, nil
}

func BenchmarkConnect_QUIC(b *testing.B) {
	_, addr, cleanup, err := setupBenchmarkEnvironment(false)
	if err != nil {
		b.Fatalf("Failed to setup server: %v", err)
	}
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Create a new ConnectionManager for each connection to simulate fresh dial
		// (In reality, we reuse CM, but we want to measure Dial time here)
		// To properly measure *just* dial, we can use the CM but force new connection?
		// or just use CM's GetOrCreate which Dials if not exists.
		// We need to close connection each time to force redial.
		
		cm := NewConnectionManager(0, "bench", false) // useTCP=false
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		
		_, err := cm.GetOrCreate(ctx, addr)
		if err != nil {
			b.Fatalf("Connect failed: %v", err)
		}
		
		cancel()
		cm.Close()
	}
}

func BenchmarkConnect_TCP(b *testing.B) {
	_, addr, cleanup, err := setupBenchmarkEnvironment(true)
	if err != nil {
		b.Fatalf("Failed to setup server: %v", err)
	}
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cm := NewConnectionManager(0, "bench", true) // useTCP=true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		
		_, err := cm.GetOrCreate(ctx, addr)
		if err != nil {
			b.Fatalf("Connect failed: %v", err)
		}
		
		cancel()
		cm.Close()
	}
}

func BenchmarkThroughput_QUIC(b *testing.B) {
	_, addr, cleanup, err := setupBenchmarkEnvironment(false)
	if err != nil {
		b.Fatalf("Failed to setup server: %v", err)
	}
	defer cleanup()

	// Establish ONE connection
	cm := NewConnectionManager(0, "bench", false)
	defer cm.Close()
	
	ctx := context.Background()
	_, err = cm.GetOrCreate(ctx, addr)
	if err != nil {
		b.Fatalf("Failed to connect: %v", err)
	}

	msg := "BenchmarkPayloadString1234567890"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cm.Send(ctx, addr, msg); err != nil {
			b.Fatalf("Send failed: %v", err)
		}
	}
}

func BenchmarkThroughput_TCP(b *testing.B) {
	_, addr, cleanup, err := setupBenchmarkEnvironment(true)
	if err != nil {
		b.Fatalf("Failed to setup server: %v", err)
	}
	defer cleanup()

	// Establish ONE connection
	cm := NewConnectionManager(0, "bench", true)
	defer cm.Close()
	
	ctx := context.Background()
	_, err = cm.GetOrCreate(ctx, addr)
	if err != nil {
		b.Fatalf("Failed to connect: %v", err)
	}

	msg := "BenchmarkPayloadString1234567890"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cm.Send(ctx, addr, msg); err != nil {
			b.Fatalf("Send failed: %v", err)
		}
	}
}
