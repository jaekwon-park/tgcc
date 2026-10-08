package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateStringRuneSafe(t *testing.T) {
	longKorean := strings.Repeat("가", 3000)

	out := truncateString(longKorean, 2000)
	if !utf8.ValidString(out) {
		t.Fatalf("truncateString produced invalid UTF-8: %q", out)
	}
	if got, want := utf8.RuneCountInString(out), 2003; got != want {
		t.Fatalf("expected %d runes (2000 + \"...\"), got %d", want, got)
	}
	if want := strings.Repeat("가", 2000) + "..."; out != want {
		t.Fatalf("unexpected truncation result")
	}
}

func TestTruncateStringDoesNotSplitMultiByteWhenUnderRuneLimit(t *testing.T) {
	s := strings.Repeat("가", 1000)
	if got := truncateString(s, 2000); got != s {
		t.Fatalf("string under rune limit must be unchanged, got %q", got)
	}
}

func TestTruncateStringShortStringUnchanged(t *testing.T) {
	if got := truncateString("hello", 2000); got != "hello" {
		t.Fatalf("short string must be unchanged, got %q", got)
	}
}
