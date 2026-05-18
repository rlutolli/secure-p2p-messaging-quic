package main

import (
	"encoding/json"
	"net"
	"reflect"
	"sort"
	"testing"
)

func TestDeduplicatePeers(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "all loopback",
			input:    []string{"127.0.0.1:1234", "127.0.0.1:5678"},
			expected: []string{"127.0.0.1:1234", "127.0.0.1:5678"},
		},
		{
			name:     "mixed removes loopback",
			input:    []string{"127.0.0.1:1234", "192.168.1.1:1234"},
			expected: []string{"192.168.1.1:1234"},
		},
		{
			name:     "only non-loopback",
			input:    []string{"192.168.1.1:1234", "10.0.0.1:5678"},
			expected: []string{"192.168.1.1:1234", "10.0.0.1:5678"},
		},
		{
			name:     "empty",
			input:    []string{},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := deduplicatePeers(tt.input)
			sort.Strings(got)
			sort.Strings(tt.expected)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("deduplicatePeers(%v) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestGetLocalAddrsMap(t *testing.T) {
	t.Parallel()
	port := 12345
	addrs := getLocalAddrsMap(port)

	loopback := net.JoinHostPort("127.0.0.1", "12345")
	if !addrs[loopback] {
		t.Errorf("expected %s to be in local addrs", loopback)
	}
}

func TestDiscoveryMessageIsRelay(t *testing.T) {
	t.Parallel()
	msg := DiscoveryMessage{
		Type:    "announce",
		Room:    "testroom",
		Port:    5000,
		Version: "0.4",
		IsRelay: true,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var parsed DiscoveryMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !parsed.IsRelay {
		t.Error("expected IsRelay to be true after round-trip")
	}
}
