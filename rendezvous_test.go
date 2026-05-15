package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRenewRegistration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/rooms/testroom" {
			t.Errorf("expected path /rooms/testroom, got %s", r.URL.Path)
		}
		var entry rendezvousEntry
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		if entry.Addr != "1.2.3.4:1234" {
			t.Errorf("expected addr 1.2.3.4:1234, got %s", entry.Addr)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RenewRegistration(ctx, server.URL, "testroom", "1.2.3.4:1234")
	if err != nil {
		t.Fatalf("RenewRegistration failed: %v", err)
	}
}

func TestRenewRegistrationErrorStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RenewRegistration(ctx, server.URL, "testroom", "1.2.3.4:1234")
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
}

func TestFetchPeers(t *testing.T) {
	t.Parallel()
	expectedPeers := []string{"1.2.3.4:1111", "5.6.7.8:2222"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/rooms/testroom" {
			t.Errorf("expected path /rooms/testroom, got %s", r.URL.Path)
		}
		resp := rendezvousRoom{Peers: expectedPeers}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers, err := FetchPeers(ctx, server.URL, "testroom")
	if err != nil {
		t.Fatalf("FetchPeers failed: %v", err)
	}
	if len(peers) != len(expectedPeers) {
		t.Fatalf("expected %d peers, got %d", len(expectedPeers), len(peers))
	}
	for i, p := range peers {
		if p != expectedPeers[i] {
			t.Errorf("peer[%d] = %q, want %q", i, p, expectedPeers[i])
		}
	}
}

func TestFetchPeersErrorStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers, err := FetchPeers(ctx, server.URL, "testroom")
	if err == nil {
		t.Fatal("expected error for 404 status")
	}
	if peers != nil {
		t.Errorf("expected nil peers, got %v", peers)
	}
}
