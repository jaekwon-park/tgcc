package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateStringUTF8Safe(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		maxLen int
	}{
		{"ascii-short", "hello", 10},
		{"ascii-long", strings.Repeat("a", 100), 50},
		{"korean-exact-boundary", strings.Repeat("한", 10), 30}, // 3 bytes per rune, exact multiple
		{"korean-mid-rune", strings.Repeat("한", 100), 100},     // 100 is not a multiple of 3
		{"mixed-mid-rune", "abc" + strings.Repeat("한글", 50), 101},
		{"emoji-boundary", strings.Repeat("🎉", 40), 10}, // 4-byte rune
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateString(tc.input, tc.maxLen)
			if !utf8.ValidString(got) {
				t.Fatalf("truncateString produced invalid UTF-8: %q", got)
			}
			if len(tc.input) <= tc.maxLen {
				if got != tc.input {
					t.Fatalf("expected no truncation, got %q", got)
				}
				return
			}
			if len(got) > tc.maxLen+3 {
				t.Fatalf("truncated result exceeds maxLen+ellipsis: got %d bytes", len(got))
			}
			if !strings.HasSuffix(got, "...") {
				t.Fatalf("expected ellipsis suffix, got %q", got)
			}
		})
	}
}

func TestTruncateUTF8TailUTF8Safe(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		maxLen int
	}{
		{"short", "hello", 10},
		{"ascii", strings.Repeat("a", 100), 50},
		{"korean-mid-rune", strings.Repeat("한", 100), 100}, // 100 not a multiple of 3
		{"emoji-boundary", strings.Repeat("🎉", 40), 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8Tail(tc.input, tc.maxLen)
			if !utf8.ValidString(got) {
				t.Fatalf("truncateUTF8Tail produced invalid UTF-8: %q", got)
			}
			if len(tc.input) <= tc.maxLen {
				if got != tc.input {
					t.Fatalf("expected no truncation, got %q", got)
				}
				return
			}
			if len(got) > tc.maxLen {
				t.Fatalf("tail result exceeds maxLen: got %d bytes", len(got))
			}
		})
	}
}
