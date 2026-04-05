package cloudreconcile

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func setupReconcileDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "weft-cloudreconcile-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	_ = tmpFile.Close()

	cleanup := db.SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		_ = os.Remove(tmpFile.Name())
	})

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestRunTwoPhasePass_OrderAndUpdates(t *testing.T) {
	database := setupReconcileDB(t)

	var phases []bool
	result := RunTwoPhasePass(context.Background(), database, Config{
		Owner:       "owner-order",
		FastTimeout: time.Second,
		FullTimeout: time.Second,
	}, func(_ context.Context, full bool) (int, error) {
		phases = append(phases, full)
		if full {
			return 7, nil
		}
		return 3, nil
	})

	if !reflect.DeepEqual(phases, []bool{false, true}) {
		t.Fatalf("phase order = %v, want [false true]", phases)
	}
	if !result.Fast.Acquired || !result.Full.Acquired {
		t.Fatalf("expected both phases to acquire lease, got fast=%+v full=%+v", result.Fast, result.Full)
	}
	if !result.Fast.Completed || !result.Full.Completed {
		t.Fatalf("expected both phases to complete, got fast=%+v full=%+v", result.Fast, result.Full)
	}
	if result.Fast.Updated != 3 || result.Full.Updated != 7 {
		t.Fatalf("updated counts = (%d, %d), want (3, 7)", result.Fast.Updated, result.Full.Updated)
	}
}

func TestRunTwoPhasePass_LeaseContentionSkipsBothPhases(t *testing.T) {
	database := setupReconcileDB(t)

	ok, err := db.AcquireAutoLease(database, DefaultLeaseScope, "other-owner", time.Minute)
	if err != nil {
		t.Fatalf("AcquireAutoLease: %v", err)
	}
	if !ok {
		t.Fatal("expected setup lease acquisition")
	}
	defer func() { _ = db.ReleaseAutoLease(database, DefaultLeaseScope, "other-owner") }()

	var calls int32
	result := RunTwoPhasePass(context.Background(), database, Config{Owner: "owner-contention"}, func(_ context.Context, _ bool) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 1, nil
	})

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("runner calls = %d, want 0", calls)
	}
	if result.Fast.Acquired || result.Full.Acquired {
		t.Fatalf("expected neither phase to acquire lease, got fast=%+v full=%+v", result.Fast, result.Full)
	}
}

func TestRunTwoPhasePass_FastPhaseTimeoutStillRunsFullPhase(t *testing.T) {
	database := setupReconcileDB(t)

	var calls int32
	result := RunTwoPhasePass(context.Background(), database, Config{
		Owner:       "owner-timeout",
		FastTimeout: 20 * time.Millisecond,
		FullTimeout: 200 * time.Millisecond,
	}, func(_ context.Context, full bool) (int, error) {
		atomic.AddInt32(&calls, 1)
		if !full {
			time.Sleep(100 * time.Millisecond)
			return 1, nil
		}
		return 2, nil
	})

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", calls)
	}
	if !result.Fast.Acquired || !result.Fast.TimedOut || result.Fast.Completed {
		t.Fatalf("unexpected fast phase result: %+v", result.Fast)
	}
	if !result.Full.Acquired || !result.Full.Completed || result.Full.TimedOut {
		t.Fatalf("unexpected full phase result: %+v", result.Full)
	}
	if result.Full.Updated != 2 {
		t.Fatalf("full phase updated = %d, want 2", result.Full.Updated)
	}
}
