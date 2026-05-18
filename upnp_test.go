package main

import (
	"context"
	"testing"
	"time"
)

func TestTryPortMapping_NoRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	externalAddr, cleanup, err := TryPortMapping(ctx, 12345, "test-mapping")
	if err != nil {
		t.Fatalf("expected no error when router not found, got: %v", err)
	}
	if externalAddr != "" {
		t.Fatalf("expected empty external address, got: %s", externalAddr)
	}
	if cleanup != nil {
		cleanup()
	}
}
