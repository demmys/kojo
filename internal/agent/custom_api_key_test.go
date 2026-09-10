package agent

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestCustomAPIKeyRoundTrip(t *testing.T) {
	creds := setupCredentialStore(t)
	if err := StoreCustomAPIKey(creds, "ag_custom", "http://peer:8888", "  sk-unsloth-test  "); err != nil {
		t.Fatalf("StoreCustomAPIKey: %v", err)
	}
	got, err := LoadCustomAPIKey(creds, "ag_custom", "http://peer:8888")
	if err != nil || got != "sk-unsloth-test" {
		t.Fatalf("LoadCustomAPIKey = %q, %v", got, err)
	}
	if other, err := LoadCustomAPIKey(creds, "ag_custom", "http://other:8888"); err != nil || other != "" {
		t.Fatalf("LoadCustomAPIKey for another URL = %q, %v", other, err)
	}
	if err := StoreCustomAPIKey(creds, "ag_custom", "", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = LoadCustomAPIKey(creds, "ag_custom", "http://peer:8888")
	if err != nil || got != "" {
		t.Fatalf("LoadCustomAPIKey after clear = %q, %v", got, err)
	}
}

func TestStoreCustomAPIKeyRejectsOversize(t *testing.T) {
	creds := setupCredentialStore(t)
	if err := StoreCustomAPIKey(creds, "ag_custom", "http://peer:8888", strings.Repeat("x", CustomAPIKeyMaxBytes+1)); err == nil {
		t.Fatal("expected oversized key to be rejected")
	}
}

func TestManagerCreateStoresCustomAPIKeyOutsideAgentRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := NewManager(logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	a, err := mgr.Create(AgentConfig{
		Name:          "Unsloth agent",
		Tool:          "custom",
		CustomBaseURL: "http://localhost:8888",
		CustomAPIKey:  "sk-unsloth-create",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	key, err := LoadCustomAPIKey(mgr.Credentials(), a.ID, a.CustomBaseURL)
	if err != nil || key != "sk-unsloth-create" {
		t.Fatalf("stored key=%q err=%v", key, err)
	}
}

func TestManagerUpdateCustomBaseURLClearsBoundKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	mgr, err := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	a, err := mgr.Create(AgentConfig{
		Name:          "URL rotation",
		Tool:          "custom",
		CustomBaseURL: "http://localhost:8888",
		CustomAPIKey:  "old-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	nextURL := "http://localhost:9999"
	if _, err := mgr.Update(a.ID, AgentUpdateConfig{CustomBaseURL: &nextURL}); err != nil {
		t.Fatal(err)
	}
	for _, baseURL := range []string{"http://localhost:8888", nextURL} {
		key, err := LoadCustomAPIKey(mgr.Credentials(), a.ID, baseURL)
		if err != nil || key != "" {
			t.Fatalf("key for %s after URL change=%q err=%v", baseURL, key, err)
		}
	}
}
