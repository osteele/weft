package db

import (
	"testing"
	"time"
)

func TestAcquireAutoLease_ContentionAndRelease(t *testing.T) {
	database := setupTestDB(t)

	ok, err := AcquireAutoLease(database, "scope-a", "owner-1", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireAutoLease(owner-1): %v", err)
	}
	if !ok {
		t.Fatalf("AcquireAutoLease(owner-1) = false, want true")
	}

	ok, err = AcquireAutoLease(database, "scope-a", "owner-2", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireAutoLease(owner-2): %v", err)
	}
	if ok {
		t.Fatalf("AcquireAutoLease(owner-2) = true, want false while owner-1 holds lease")
	}

	if err := ReleaseAutoLease(database, "scope-a", "owner-1"); err != nil {
		t.Fatalf("ReleaseAutoLease(owner-1): %v", err)
	}

	ok, err = AcquireAutoLease(database, "scope-a", "owner-2", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireAutoLease(owner-2) after release: %v", err)
	}
	if !ok {
		t.Fatalf("AcquireAutoLease(owner-2) after release = false, want true")
	}
}
