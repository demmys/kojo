package agent

import (
	"context"
	"testing"
	"time"
)

func TestWaitBusyClear_NotBusy(t *testing.T) {
	m := newTestManager(t)
	if err := m.waitBusyClear("ag_1"); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestWaitBusyClear_BecomesFree(t *testing.T) {
	m := newTestManager(t)
	_, cancel := context.WithCancel(context.Background())
	m.busy["ag_1"] = busyEntry{cancel: cancel, startedAt: time.Now()}

	// Clear busy after 200ms
	go func() {
		time.Sleep(200 * time.Millisecond)
		m.busyMu.Lock()
		delete(m.busy, "ag_1")
		m.busyMu.Unlock()
	}()

	if err := m.waitBusyClear("ag_1"); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestWaitBusyClear_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow test")
	}

	m := newTestManager(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.busy["ag_1"] = busyEntry{cancel: cancel, startedAt: time.Now()}

	err := m.waitBusyClear("ag_1")
	if err == nil {
		t.Error("expected error after timeout")
	}
}

func TestAcquireResetGuard_Success(t *testing.T) {
	m := newTestManager(t)
	cleanup, err := m.acquireResetGuard("ag_1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Should be marked as resetting
	m.busyMu.Lock()
	if !m.resetting["ag_1"] {
		t.Error("expected resetting flag to be set")
	}
	m.busyMu.Unlock()

	cleanup()

	m.busyMu.Lock()
	if m.resetting["ag_1"] {
		t.Error("expected resetting flag to be cleared after cleanup")
	}
	m.busyMu.Unlock()
}

func TestAcquireResetGuard_AlreadyResetting(t *testing.T) {
	m := newTestManager(t)
	m.resetting["ag_1"] = true

	_, err := m.acquireResetGuard("ag_1")
	if err == nil {
		t.Error("expected error when already resetting")
	}
}

func TestAcquireResetGuard_CancelsBusy(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy["ag_1"] = busyEntry{cancel: cancel, startedAt: time.Now()}

	cleanup, err := m.acquireResetGuard("ag_1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer cleanup()

	// Context should be cancelled
	select {
	case <-ctx.Done():
		// expected
	default:
		t.Error("expected busy context to be cancelled")
	}
}
