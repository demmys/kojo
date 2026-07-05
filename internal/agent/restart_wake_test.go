package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestRestartWake_ArmAndConsume: the marker round-trips through kv and
// is consumed at-most-once — a second ConsumeRestartWake is a no-op.
func TestRestartWake_ArmAndConsume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.ArmRestartWake("ag_wake_missing"); err != nil {
		t.Fatalf("arm: %v", err)
	}
	rec, err := m.Store().GetKV(context.Background(), "system", "restart_wake")
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if rec.Value != "ag_wake_missing" {
		t.Fatalf("marker = %q", rec.Value)
	}

	// Consume: the agent doesn't exist, so the chat fails (logged), but
	// the marker MUST be cleared regardless — at-most-once.
	m.ConsumeRestartWake("vtest", time.Now().Add(time.Minute))
	if _, err := m.Store().GetKV(context.Background(), "system", "restart_wake"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("marker not cleared after consume: %v", err)
	}

	// Second consume with no marker: no panic, still nothing.
	m.ConsumeRestartWake("vtest", time.Now().Add(time.Minute))
}

// TestRestartWake_BootFence: a marker written AFTER the consumer's boot
// time belongs to the next process and must be left in place.
func TestRestartWake_BootFence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.ArmRestartWake("ag_future"); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Boot time in the past → marker is newer → must survive.
	m.ConsumeRestartWake("vtest", time.Now().Add(-time.Minute))
	rec, err := m.Store().GetKV(context.Background(), "system", "restart_wake")
	if err != nil {
		t.Fatalf("marker was consumed despite being newer than boot: %v", err)
	}
	if rec.Value != "ag_future" {
		t.Fatalf("marker = %q", rec.Value)
	}
}

// TestWakeChat_Rejections mirrors Checkin's entry contract.
func TestWakeChat_Rejections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.WakeChat("ag_nope", "hi"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing agent: err = %v, want ErrAgentNotFound", err)
	}
}
