package main

import (
	"testing"
	"unicode"
)

func TestGenerateAlias(t *testing.T) {
	t.Parallel()
	for i := 0; i < 10; i++ {
		alias := generateAlias()
		if alias == "" {
			t.Fatal("generateAlias returned empty string")
		}
		hasDigit := false
		for _, r := range alias {
			if unicode.IsDigit(r) {
				hasDigit = true
				break
			}
		}
		if !hasDigit {
			t.Errorf("expected alias to contain a digit, got %q", alias)
		}
	}
}

func TestSanitiseField(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no special chars", "hello", "hello"},
		{"pipe chars", "hel|lo|world", "helloworld"},
		{"newline", "hello\nworld", "helloworld"},
		{"carriage return", "hello\rworld", "helloworld"},
		{"combined", "a|\nb\rc", "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitiseField(tt.input)
			if got != tt.expected {
				t.Errorf("sanitiseField(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestRandIntInBounds(t *testing.T) {
	t.Parallel()
	max := 100
	for i := 0; i < 1000; i++ {
		n := randInt(max)
		if n < 0 || n >= max {
			t.Fatalf("randInt(%d) = %d, out of bounds", max, n)
		}
	}
}
