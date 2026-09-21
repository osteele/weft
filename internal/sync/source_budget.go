package sync

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Source upload budgets.
//
// A flat wall-clock budget cannot bound a source pin correctly, because the
// size guard and the budget answer to different quantities: MaxSourceTarballBytes
// admits 500 MB, while a fixed 10-minute cap buys only what the uplink delivers
// in ten minutes — about 37 MB on a measured 63 KB/s link. Payloads the size
// guard admits could not be delivered, and the mismatch surfaced as a canceled
// PutObject rather than as a timeout.
//
// The budget is therefore progress-based rather than duration-based. Silence is
// what gets bounded: an upload that keeps moving bytes is allowed to finish
// however slowly the link requires, and one that stops moving them fails
// promptly and by name. SourceUploadBudget is a backstop for the pathological
// case of a link that dribbles bytes indefinitely; it scales with the payload,
// so it cannot contradict the size guard the way a fixed cap did.
// SourceUploadStallTimeout bounds how long a single object upload may go
// without moving a byte before it is abandoned. Large objects upload via
// multipart with per-part progress feeds (PutObjectWithPartProgress); one
// default 8 MiB part over the floor uplink (32 KB/s) takes ~256 s, so the
// window must exceed a single part transfer for a healthy multipart upload
// not to trip between feeds. 5 minutes covers the floor uplink with margin.
// Variable so tests can shorten the window; not a tuning knob.
var SourceUploadStallTimeout = 5 * time.Minute

const (
	// sourceUploadBudgetBase covers connection setup and request overhead that
	// does not scale with payload size.
	sourceUploadBudgetBase = 2 * time.Minute

	// sourceUploadFloorBytesPerSec is the slowest sustained uplink the total
	// backstop will wait out. It is deliberately well below observed
	// throughput (63 KB/s measured on the reporting workstation): the stall
	// timeout is the instrument that catches a wedged transfer, and this
	// bound exists only so a dribbling link cannot block forever.
	sourceUploadFloorBytesPerSec = 32 * 1024
)

// ErrSourceUploadStalled reports that a source upload stopped transferring
// bytes for longer than SourceUploadStallTimeout. Distinct from a canceled
// context so callers can tell a wedged uplink from an abandoned command.
var ErrSourceUploadStalled = errors.New("source upload stalled")

// ErrSourceUploadBudgetExceeded reports that the upload phase as a whole
// outran the size-derived backstop returned by SourceUploadBudget.
var ErrSourceUploadBudgetExceeded = errors.New("source upload exceeded its size-derived budget")

// SourceUploadBudget returns the total time allowed to upload pendingBytes.
// It scales with the payload so that any size the tarball guard admits has a
// budget that can deliver it.
func SourceUploadBudget(pendingBytes int64) time.Duration {
	if pendingBytes <= 0 {
		return sourceUploadBudgetBase
	}
	transfer := time.Duration(pendingBytes/sourceUploadFloorBytesPerSec) * time.Second
	return sourceUploadBudgetBase + transfer
}

// stallWatchedReader wraps an upload body and cancels the upload when no byte
// is read for stallAfter. It preserves io.Seeker so the AWS SDK can rewind the
// body to retry a request; a rewind counts as progress, since it means the
// request is being reissued rather than hanging.
type stallWatchedReader struct {
	inner      io.ReadSeeker
	stallAfter time.Duration
	cancel     func()

	mu       sync.Mutex
	lastMove time.Time
	tripped  bool

	stopOnce sync.Once
	done     chan struct{}
}

func newStallWatchedReader(inner io.ReadSeeker, stallAfter time.Duration, cancel func()) *stallWatchedReader {
	r := &stallWatchedReader{
		inner:      inner,
		stallAfter: stallAfter,
		cancel:     cancel,
		lastMove:   time.Now(),
		done:       make(chan struct{}),
	}
	go r.watch()
	return r
}

func (r *stallWatchedReader) watch() {
	// Poll at a fraction of the stall window so the observed delay before
	// tripping stays close to stallAfter.
	tick := r.stallAfter / 4
	if tick <= 0 {
		tick = time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.mu.Lock()
			idle := time.Since(r.lastMove)
			if idle >= r.stallAfter {
				r.tripped = true
				r.mu.Unlock()
				r.cancel()
				return
			}
			r.mu.Unlock()
		}
	}
}

func (r *stallWatchedReader) note() {
	r.mu.Lock()
	r.lastMove = time.Now()
	r.mu.Unlock()
}

// Progress feeds the watchdog from transfer progress that does not show up
// as a read of this reader — multipart part completions. The SDK may serve a
// part from an internal buffer, so read-derived progress goes silent during
// a slow part transfer; this keeps a healthy upload from tripping the stall
// window.
func (r *stallWatchedReader) Progress() {
	r.note()
}

func (r *stallWatchedReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.note()
	}
	return n, err
}

func (r *stallWatchedReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := r.inner.Seek(offset, whence)
	if err == nil {
		r.note()
	}
	return pos, err
}

// stop ends the watchdog. Safe to call more than once.
func (r *stallWatchedReader) stop() {
	r.stopOnce.Do(func() { close(r.done) })
}

// stalled reports whether the watchdog canceled the upload.
func (r *stallWatchedReader) stalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tripped
}

// describeStall renders the user-facing remediation for a stalled upload.
func describeStall(label string, stallAfter time.Duration) error {
	return fmt.Errorf("%w: %s moved no data for %s; the uplink appears wedged rather than slow",
		ErrSourceUploadStalled, label, stallAfter)
}
