package chathistory

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestScanJSONLLines_BasicAndEOF(t *testing.T) {
	input := "line one\nline two\nline three no newline"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 || got[0] != "line one" || got[1] != "line two" || got[2] != "line three no newline" {
		t.Errorf("unexpected lines: %#v", got)
	}
}

// TestScanJSONLLines_LongLineCrossingBufioBuffer ensures that lines longer
// than bufio's default buffer (4 KiB) are correctly reassembled across
// ReadSlice/ErrBufferFull boundaries.
func TestScanJSONLLines_LongLineCrossingBufioBuffer(t *testing.T) {
	long := strings.Repeat("a", 100_000) // > default bufio buffer
	input := "first\n" + long + "\nlast\n"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(got))
	}
	if got[1] != long {
		t.Errorf("long line corrupted: len=%d want=%d", len(got[1]), len(long))
	}
	if got[0] != "first" || got[2] != "last" {
		t.Errorf("surrounding lines: %q / %q", got[0], got[2])
	}
}

// TestScanJSONLLines_OversizeLineReturnsErr is the security-relevant
// behavior of this helper: a single line that exceeds MaxJSONLLineBytes
// must abort with ErrLineTooLarge instead of growing the buffer
// unboundedly.
func TestScanJSONLLines_OversizeLineReturnsErr(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), MaxJSONLLineBytes+1)
	var got int
	err := ScanJSONLLines(bytes.NewReader(huge), func(line []byte) {
		got++
	})
	if !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("expected ErrLineTooLarge, got %v", err)
	}
	if got != 0 {
		t.Errorf("onLine should not be called for an oversize line, got %d", got)
	}
}

// TestScanJSONLLines_EmptyLinesIgnored ensures the helper treats lone
// "\n\n" runs without crashing and passes through empty lines.
func TestScanJSONLLines_EmptyLinesIgnored(t *testing.T) {
	input := "a\n\nb\n"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "b" {
		t.Errorf("unexpected lines: %#v", got)
	}
}

// TestLoadHistory_OversizeLinePropagates ensures that a corrupted history
// file with an oversize line surfaces as an error to LoadHistory's caller.
