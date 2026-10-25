package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// freshGateEnv points the platform config root at a temp dir so
// configdir.V0Path / V1Path resolve under it. Returns (v0, v1).
func freshGateEnv(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("APPDATA", root)
	return filepath.Join(root, "kojo"), filepath.Join(root, "kojo-v1")
}

func gateLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runGate(t *testing.T, flags migrationFlags) bool {
	t.Helper()
	return applyStartupGate(context.Background(), flags, gateLogger(), "test")
}

func mustFreshMarker(t *testing.T, v1 string) {
	t.Helper()
	cf, err := readCompleteFile(v1)
	if err != nil {
		t.Fatalf("read complete file: %v", err)
	}
	if !cf.FreshInstall || cf.V1SchemaVersion <= 0 || cf.MigratorVersion != "test" || cf.V0Path != "" {
		t.Fatalf("unexpected marker: %+v", cf)
	}
	if err := verifyCompleteFile(filepath.Join(v1, migrate.CompleteFileName)); err != nil {
		t.Fatalf("verifyCompleteFile: %v", err)
	}
}

// openV1Store simulates a runtime boot materializing kojo.db.
func openV1Store(t *testing.T, v1 string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()
}

// First-time install must publish the marker so the second boot works.
func TestStartupGate_FreshInstallBootsTwice(t *testing.T) {
	_, v1 := freshGateEnv(t)
	if !runGate(t, migrationFlags{}) {
		t.Fatal("first boot did not proceed")
	}
	mustFreshMarker(t, v1)
	openV1Store(t, v1)
	if !runGate(t, migrationFlags{}) {
		t.Fatal("second boot did not proceed")
	}
}

// A v1 dir left by a pre-fix build (kojo.db, no marker, no v0) is adopted.
func TestStartupGate_AdoptsUnmarkedInstall(t *testing.T) {
	_, v1 := freshGateEnv(t)
	openV1Store(t, v1)
	if err := os.MkdirAll(filepath.Join(v1, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !runGate(t, migrationFlags{}) {
		t.Fatal("boot did not proceed")
	}
	mustFreshMarker(t, v1)
}

// --fresh next to v0 data must keep booting, with and without --fresh.
func TestStartupGate_FreshWithV0BootsAgain(t *testing.T) {
	v0, v1 := freshGateEnv(t)
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatal(err)
	}
	if !runGate(t, migrationFlags{fresh: true}) {
		t.Fatal("--fresh boot did not proceed")
	}
	mustFreshMarker(t, v1)
	openV1Store(t, v1)
	if !runGate(t, migrationFlags{}) {
		t.Fatal("normal boot after --fresh did not proceed")
	}
	if !runGate(t, migrationFlags{fresh: true}) {
		t.Fatal("repeated --fresh boot did not proceed")
	}
}

// --fresh adopts an unmarked v1 dir left by a pre-fix --fresh boot.
func TestStartupGate_FreshAdoptsUnmarkedWithV0(t *testing.T) {
	v0, v1 := freshGateEnv(t)
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatal(err)
	}
	openV1Store(t, v1)
	if !runGate(t, migrationFlags{fresh: true}) {
		t.Fatal("--fresh boot did not proceed")
	}
	mustFreshMarker(t, v1)
}

func TestIsUnmarkedV1Install(t *testing.T) {
	_, v1 := freshGateEnv(t)
	if isUnmarkedV1Install(v1) {
		t.Fatal("missing dir reported as unmarked install")
	}
	openV1Store(t, v1)
	if !isUnmarkedV1Install(v1) {
		t.Fatal("kojo.db-only dir not reported as unmarked install")
	}
	lock := filepath.Join(v1, migrate.LockFileName)
	if err := os.WriteFile(lock, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isUnmarkedV1Install(v1) {
		t.Fatal("dir with migration lock reported as unmarked install")
	}
}
