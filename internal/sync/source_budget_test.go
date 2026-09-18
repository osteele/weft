package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/r2"
)

// stallingStore accepts a PutObject and then never reads the body, modelling a
// wedged uplink: the request is open, no bytes move.
type stallingStore struct {
	started chan struct{}
}

func (s *stallingStore) ObjectExists(context.Context, string) (bool, error) { return false, nil }
func (s *stallingStore) ListObjectsLimited(context.Context, string, int) ([]r2.ObjectInfo, bool, error) {
	return nil, false, nil
}

func (s *stallingStore) PutObject(ctx context.Context, _ string, _ io.Reader, _ string) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

func (s *stallingStore) PutObjectConditional(context.Context, string, io.Reader, string, string, string) (string, error) {
	return "", nil
}

// trickleStore reads the body in small steps with pauses shorter than the
// stall window, modelling a slow but healthy uplink.
type trickleStore struct {
	step  time.Duration
	steps int
}

func (s *trickleStore) ObjectExists(context.Context, string) (bool, error) { return false, nil }
func (s *trickleStore) ListObjectsLimited(context.Context, string, int) ([]r2.ObjectInfo, bool, error) {
	return nil, false, nil
}

func (s *trickleStore) PutObject(ctx context.Context, _ string, body io.Reader, _ string) error {
	buf := make([]byte, 1)
	for i := 0; i < s.steps; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.step):
		}
		if _, err := body.Read(buf); err != nil && err != io.EOF {
			return err
		}
	}
	_, err := io.Copy(io.Discard, body)
	return err
}

func (s *trickleStore) PutObjectConditional(context.Context, string, io.Reader, string, string, string) (string, error) {
	return "", nil
}

func testUploadObject(t *testing.T, payload []byte) sourceUploadObject {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return sourceUploadObject{
		key:         "sources/v2/sha256/test.tar.gz",
		digest:      "test",
		label:       "source root /tmp/test",
		contentType: "application/gzip",
		size:        int64(len(payload)),
		open: func() (*os.File, func(), error) {
			file, err := os.Open(path)
			return file, func() {}, err
		},
	}
}

// TestSourceUploadBudgetCoversTheSizeGuard is the wb149 regression on the
// arithmetic: the old flat 10-minute budget bought ~37 MB on the reporting
// workstation's link while the size guard admitted 500 MB. Whatever the guard
// admits must now have a budget that can deliver it at the floor rate.
func TestSourceUploadBudgetCoversTheSizeGuard(t *testing.T) {
	budget := SourceUploadBudget(MaxSourceTarballBytes)
	needed := time.Duration(MaxSourceTarballBytes/sourceUploadFloorBytesPerSec) * time.Second
	if budget < needed {
		t.Errorf("budget %s cannot deliver the %d-byte size guard at the floor rate (%s needed)",
			budget, int64(MaxSourceTarballBytes), needed)
	}
	if budget <= 10*time.Minute {
		t.Errorf("budget for the full size guard is %s, no better than the flat cap it replaced", budget)
	}
}

func TestSourceUploadBudgetBaseForEmptyPayload(t *testing.T) {
	if got := SourceUploadBudget(0); got != sourceUploadBudgetBase {
		t.Errorf("SourceUploadBudget(0) = %s, want %s", got, sourceUploadBudgetBase)
	}
	if SourceUploadBudget(100*1024*1024) <= SourceUploadBudget(1024) {
		t.Error("budget does not grow with payload size")
	}
}

// TestUploadSourceObjectFailsFastOnStall covers the reported failure shape: a
// transfer that moves no bytes must fail promptly, by name, rather than run out
// an unrelated wall-clock budget and surface as a canceled PutObject.
func TestUploadSourceObjectFailsFastOnStall(t *testing.T) {
	original := SourceUploadStallTimeout
	SourceUploadStallTimeout = 150 * time.Millisecond
	t.Cleanup(func() { SourceUploadStallTimeout = original })

	store := &stallingStore{started: make(chan struct{})}
	object := testUploadObject(t, []byte("payload"))

	start := time.Now()
	err := uploadSourceObject(context.Background(), store, object)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSourceUploadStalled) {
		t.Fatalf("expected ErrSourceUploadStalled, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("stall detection took %s, expected to trip near the stall window", elapsed)
	}
}

// TestUploadSourceObjectToleratesSlowProgress is the other half: the watchdog
// bounds silence, not slowness. A link delivering bytes steadily for longer
// than the stall window must be allowed to finish.
func TestUploadSourceObjectToleratesSlowProgress(t *testing.T) {
	original := SourceUploadStallTimeout
	SourceUploadStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { SourceUploadStallTimeout = original })

	store := &trickleStore{step: 50 * time.Millisecond, steps: 12}
	object := testUploadObject(t, make([]byte, 4096))

	start := time.Now()
	if err := uploadSourceObject(context.Background(), store, object); err != nil {
		t.Fatalf("slow but progressing upload failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < SourceUploadStallTimeout {
		t.Fatalf("upload finished in %s, too fast to have outlived the stall window", elapsed)
	}
}

// TestUploadSourceObjectPropagatesCallerCancellation keeps a user abort
// distinguishable from a wedged uplink.
func TestUploadSourceObjectPropagatesCallerCancellation(t *testing.T) {
	original := SourceUploadStallTimeout
	SourceUploadStallTimeout = time.Hour
	t.Cleanup(func() { SourceUploadStallTimeout = original })

	store := &stallingStore{started: make(chan struct{})}
	object := testUploadObject(t, []byte("payload"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-store.started
		cancel()
	}()

	err := uploadSourceObject(ctx, store, object)
	if errors.Is(err, ErrSourceUploadStalled) {
		t.Fatalf("caller cancellation misreported as a stall: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
