package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
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

func (s *stallingStore) PutObjectWithPartProgress(ctx context.Context, key string, body io.Reader, contentType string, onPart func(int64)) error {
	return s.PutObject(ctx, key, body, contentType)
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

func (s *trickleStore) PutObjectWithPartProgress(ctx context.Context, key string, body io.Reader, contentType string, onPart func(int64)) error {
	return s.PutObject(ctx, key, body, contentType)
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

// partFeedStore models the multipart shape the wb158 review flagged: the
// body is consumed once up front (an SDK serving a part from an internal
// buffer), the transfer then runs slowly with no further body reads, and
// per-part progress feeds keep the caller informed.
type partFeedStore struct {
	feedEvery time.Duration
	feeds     int
	transfer  time.Duration // silent tail after the feeds; models the wire time of the final part
}

func (s *partFeedStore) ObjectExists(context.Context, string) (bool, error) { return false, nil }
func (s *partFeedStore) ListObjectsLimited(context.Context, string, int) ([]r2.ObjectInfo, bool, error) {
	return nil, false, nil
}

func (s *partFeedStore) PutObjectWithPartProgress(ctx context.Context, _ string, body io.Reader, _ string, onPart func(int64)) error {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	for i := 0; i < s.feeds; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.feedEvery):
		}
		onPart(16 * 1024 * 1024)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.transfer):
	}
	return nil
}

func (s *partFeedStore) PutObject(context.Context, string, io.Reader, string) error {
	return errors.New("unexpected single-shot PutObject call")
}

func (s *partFeedStore) PutObjectConditional(context.Context, string, io.Reader, string, string, string) (string, error) {
	return "", nil
}

// TestUploadSourceObjectToleratesSlowMultipartWithProgressFeeds pins the
// wb158 review fix: a multipart transfer that reads the body only up front
// must survive when per-part progress feeds arrive, because the SDK may
// serve a part from an internal buffer and read-derived progress goes
// silent for the whole part transfer.
func TestUploadSourceObjectToleratesSlowMultipartWithProgressFeeds(t *testing.T) {
	original := SourceUploadStallTimeout
	SourceUploadStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { SourceUploadStallTimeout = original })

	store := &partFeedStore{feedEvery: 50 * time.Millisecond, feeds: 6, transfer: 50 * time.Millisecond}
	object := testUploadObject(t, make([]byte, 4096))

	if err := uploadSourceObject(context.Background(), store, object); err != nil {
		t.Fatalf("multipart upload with progress feeds failed: %v", err)
	}
}

// TestUploadSourceObjectStillTripsWithoutProgressFeeds is the complement: the
// progress feed is what keeps the watchdog alive. The same slow transfer
// without feeds must still fail as stalled.
func TestUploadSourceObjectStillTripsWithoutProgressFeeds(t *testing.T) {
	original := SourceUploadStallTimeout
	SourceUploadStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { SourceUploadStallTimeout = original })

	store := &partFeedStore{feeds: 0, transfer: 600 * time.Millisecond}
	object := testUploadObject(t, make([]byte, 4096))

	if err := uploadSourceObject(context.Background(), store, object); !errors.Is(err, ErrSourceUploadStalled) {
		t.Fatalf("expected ErrSourceUploadStalled without progress feeds, got %v", err)
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

	// The bubble fakes the clock, so the 150 ms window elapses instantly and
	// the test is free of scheduling flake regardless of machine load.
	synctest.Test(t, func(t *testing.T) {
		err := uploadSourceObject(context.Background(), store, object)
		if !errors.Is(err, ErrSourceUploadStalled) {
			t.Errorf("expected ErrSourceUploadStalled, got %v", err)
		}
	})
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

	synctest.Test(t, func(t *testing.T) {
		if err := uploadSourceObject(context.Background(), store, object); err != nil {
			t.Errorf("slow but progressing upload failed: %v", err)
		}
	})
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
	var err error
	synctest.Test(t, func(t *testing.T) {
		go func() {
			<-store.started
			cancel()
		}()
		err = uploadSourceObject(ctx, store, object)
	})

	if errors.Is(err, ErrSourceUploadStalled) {
		t.Errorf("caller cancellation misreported as a stall: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
