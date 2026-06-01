package main

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errorClass
	}{
		{"nil", nil, errBenign},
		{"io.EOF", io.EOF, errBenign},
		{"app error 0x0", errors.New("Application error 0x0 (remote): loadtest done"), errBenign},
		{"closed conn", errors.New("use of closed network connection"), errBenign},
		{"context canceled", errors.New("context canceled"), errBenign},
		{"refused", errors.New("dial tcp: connect: connection refused"), errTransient},
		{"quic refused", errors.New("CONNECTION_REFUSED (remote)"), errTransient},
		{"io timeout", errors.New("read: i/o timeout"), errTransient},
		{"reset", errors.New("read: connection reset by peer"), errTransient},
		{"auth failed", errors.New("auth failed: AUTH:FAILED|bad password"), errReal},
		{"unknown", errors.New("something unexpected happened"), errReal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyError(tt.err); got != tt.want {
				t.Errorf("classifyError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsTransientDialError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"refused is transient", errors.New("connection refused"), true},
		{"timeout is transient", errors.New("i/o timeout"), true},
		{"eof is not transient", io.EOF, false},
		{"unknown is not transient", errors.New("boom"), false},
		{"nil is not transient", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientDialError(tt.err); got != tt.want {
				t.Errorf("isTransientDialError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestEffectiveStagger(t *testing.T) {
	tests := []struct {
		name     string
		explicit time.Duration
		n        int
		want     time.Duration
	}{
		{"explicit wins", 20 * time.Millisecond, 10, 20 * time.Millisecond},
		{"explicit wins at scale", 1 * time.Millisecond, 1000, 1 * time.Millisecond},
		{"auto off below 500", 0, 100, 0},
		{"auto on at 500", 0, 500, 5 * time.Millisecond},
		{"auto on above 500", 0, 1000, 5 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveStagger(tt.explicit, tt.n); got != tt.want {
				t.Errorf("effectiveStagger(%v, %d) = %v, want %v", tt.explicit, tt.n, got, tt.want)
			}
		})
	}
}

func TestErrorStatsRealCount(t *testing.T) {
	es := newErrorStats()
	es.record(io.EOF)                                  // benign
	es.record(errors.New("connection refused"))        // transient
	es.record(errors.New("connection reset by peer"))  // transient
	es.record(errors.New("unexpected protocol error")) // real
	es.record(nil)                                     // ignored

	if got := es.realCount(); got != 3 {
		t.Errorf("realCount() = %d, want 3 (2 transient + 1 real)", got)
	}
	if es.counts[errBenign] != 1 {
		t.Errorf("benign count = %d, want 1", es.counts[errBenign])
	}
}

func TestBuildPayloadSize(t *testing.T) {
	tests := []struct {
		name  string
		msgID string
		size  int
	}{
		{"normal padding", "peer-0-0", 100},
		{"exact prefix", "peer-1-2", len("peer-1-2|")},
		{"tiny size", "peer-0-0", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPayload(tt.msgID, tt.size)
			if tt.size > len(tt.msgID)+1 && len(got) != tt.size {
				t.Errorf("buildPayload(%q,%d) len=%d, want %d", tt.msgID, tt.size, len(got), tt.size)
			}
		})
	}
}
