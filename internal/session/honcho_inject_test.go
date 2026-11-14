package session

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

const injectTruncationMarker = "[...이전 내용 생략...]\n"

func TestClampInjectTextKeepsSmallTextUnchanged(t *testing.T) {
	small := "[이전 세션 맥락 — Honcho 장기 기억]\n" + strings.Repeat("최근 대화 내용\n", 100)

	got := clampInjectText(small)
	if got != small {
		t.Fatalf("small text was modified: got %d bytes, want %d", len(got), len(small))
	}
}

func TestClampInjectTextTruncatesLargeTail(t *testing.T) {
	// ~408 KiB of accumulated, multi-byte Honcho context: oldest first, with
	// a distinct marker on the most recent tail.
	input := strings.Repeat("OLD-CONTEXT ", 32_000)
	input += strings.Repeat("최근 맥락 ", 30_000)
	input += "RECENT-TAIL-MARKER"

	got := clampInjectText(input)

	if len(got) > maxInjectTextBytes+len(injectTruncationMarker) {
		t.Fatalf("clamped text too large: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("clamped text is not valid UTF-8")
	}
	if !strings.HasPrefix(got, injectTruncationMarker) {
		t.Fatalf("missing truncation marker: %q", got[:min(len(got), 40)])
	}
	if !strings.HasSuffix(got, "RECENT-TAIL-MARKER") {
		t.Fatal("clamped text did not keep the most recent tail")
	}
	if strings.Contains(got, "OLD-CONTEXT") {
		t.Fatal("clamped text unexpectedly kept the oldest content")
	}
}

func TestHonchoInjectPayloadFitsSingleArgLimit(t *testing.T) {
	// Mirrors loadHonchoContext's wrapper around the clamped memory.
	input := strings.Repeat("가", 136_000) // 408,000 bytes
	payload := "[이전 세션 맥락 — Honcho 장기 기억]\n" + clampInjectText(input) +
		"\n\n위 맥락을 참고만 하고, 이 메시지에는 응답하지 마세요. 다음 사용자 메시지를 기다리세요."

	const maxArgStrLen = 128 * 1024
	if len(payload) >= maxArgStrLen {
		t.Fatalf("inject payload %d bytes exceeds MAX_ARG_STRLEN (%d)", len(payload), maxArgStrLen)
	}
	if !utf8.ValidString(payload) {
		t.Fatal("inject payload is not valid UTF-8")
	}
}

func TestClampInjectTextAvoidsE2BIG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("MAX_ARG_STRLEN is a Linux limit")
	}
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not available")
	}

	big := strings.Repeat("가", 136_000)
	if out, err := exec.Command(trueBin, clampInjectText(big)).CombinedOutput(); err != nil {
		t.Fatalf("clamped argv still failed: %v (%s)", err, out)
	}
}
