package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRunCLIGenerateTimeoutSurfacesDeadline verifies that a deadline
// kill is wrapped in context.DeadlineExceeded (not just exec's opaque
// "signal: killed"), so resolveTurnEffort can classify it as expected
// degradation rather than a genuine CLI failure.
func TestRunCLIGenerateTimeoutSurfacesDeadline(t *testing.T) {
	_, err := runCLIGenerateTimeout(context.Background(),
		"sleep", []string{"5"}, "", "", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected error from killed process")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded in chain, got %v", err)
	}
}
