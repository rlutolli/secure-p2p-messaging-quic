package main

import "testing"

func TestParsePortArg(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"zero is valid (ephemeral)", "0", 0, false},
		{"typical port", "33333", 33333, false},
		{"max port", "65535", 65535, false},
		{"whitespace trimmed", "  8080 ", 8080, false},
		{"too large", "65536", 0, true},
		{"negative", "-1", 0, true},
		{"not a number", "abc", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortArg(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePortArg(%q) expected error, got nil", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortArg(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parsePortArg(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
