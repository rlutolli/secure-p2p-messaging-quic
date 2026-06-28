package main

import (
	"sync"
	"time"
)

// ReplayProtector tracks seen nonces per connection to reject replays.
type ReplayProtector struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	maxAge time.Duration
	done   chan struct{}
}

// NewReplayProtector creates a replay protector that forgets nonces after maxAge.
// A background goroutine cleans expired nonces every 60 seconds.
func NewReplayProtector(maxAge time.Duration) *ReplayProtector {
	rp := &ReplayProtector{
		seen:   make(map[string]time.Time),
		maxAge: maxAge,
		done:   make(chan struct{}),
	}
	go rp.cleanup()
	return rp
}

// Check returns true if the nonce is fresh (not seen before), recording it.
// Returns false if the nonce has already been seen (replay attack).
func (rp *ReplayProtector) Check(nonce string) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if _, seen := rp.seen[nonce]; seen {
		return false
	}
	rp.seen[nonce] = time.Now()
	return true
}

// cleanup periodically removes nonces older than maxAge to bound memory.
func (rp *ReplayProtector) cleanup() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-rp.done:
			return
		case <-ticker.C:
			rp.mu.Lock()
			cutoff := time.Now().Add(-rp.maxAge)
			for nonce, ts := range rp.seen {
				if ts.Before(cutoff) {
					delete(rp.seen, nonce)
				}
			}
			rp.mu.Unlock()
		}
	}
}

// Close stops the background cleanup goroutine.
func (rp *ReplayProtector) Close() {
	close(rp.done)
}
