package configdir

import (
	"testing"
)

func TestAcquireReleaseRelock(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	if _, err := Acquire(dir); err == nil {
		t.Fatal("second Acquire on held lock should fail")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	again.Release()
}

func TestReleaseNilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("Release on nil Lock: %v", err)
	}
}
