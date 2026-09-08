package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrAlreadyRecorded reports that a nonce was recorded by someone else between
// this caller's Seen check and its Record. It is a lost race, not a failure:
// the submission is already accounted for and must not be admitted again.
var ErrAlreadyRecorded = errors.New("edge: nonce already recorded")

// FileSeenSet records processed nonces as files in a directory.
//
// Recording is a create-exclusive write, so two hub processes racing on the
// same nonce cannot both conclude it is fresh.
type FileSeenSet struct {
	dir string
}

func NewFileSeenSet(dir string) (*FileSeenSet, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create seen-set directory %s: %w", dir, err)
	}
	return &FileSeenSet{dir: dir}, nil
}

func (s *FileSeenSet) path(nonce string) (string, error) {
	// A nil *FileSeenSet stored in a SeenSet interface is not a nil interface,
	// so the callers' `seen != nil` guards let it through. Guard on the
	// receiver instead of trusting them.
	if s == nil {
		return "", fmt.Errorf("seen-set is not configured")
	}
	if nonce == "" || filepath.Base(nonce) != nonce {
		return "", fmt.Errorf("invalid nonce %q", nonce)
	}
	return filepath.Join(s.dir, nonce+".json"), nil
}

// Seen reports whether the nonce was already processed. A stat failure other
// than "not present" is returned as an error rather than as false, so an
// unreadable seen-set cannot be mistaken for an empty one.
func (s *FileSeenSet) Seen(nonce string) (bool, error) {
	path, err := s.path(nonce)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("check seen-set for nonce %s: %w", nonce, err)
}

type seenRecord struct {
	Nonce       string    `json:"nonce"`
	SubmittedAt time.Time `json:"submitted_at"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// Record marks a nonce processed. Callers must call this before admitting the
// submission, so a crash between the two cannot admit the same work twice.
func (s *FileSeenSet) Record(nonce string, submittedAt time.Time) error {
	path, err := s.path(nonce)
	if err != nil {
		return err
	}
	data, err := json.Marshal(seenRecord{
		Nonce:       nonce,
		SubmittedAt: submittedAt.UTC(),
		RecordedAt:  time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("encode seen record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		// Report the collision rather than swallowing it. Two hub processes
		// can both pass the Seen check before either records, and if the
		// loser of the exclusive create is told "fine" it admits the same
		// submission a second time — which for a rental submission means
		// paying twice.
		return fmt.Errorf("%s: %w", nonce, ErrAlreadyRecorded)
	}
	if err != nil {
		return fmt.Errorf("record nonce %s: %w", nonce, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write seen record %s: %w", nonce, err)
	}
	return nil
}

// Forget removes a record when admission could not reach its durable effect.
// Pollers use it only to roll back a synchronous hub-side failure such as a
// locked database; protocol refusals remain terminal.
func (s *FileSeenSet) Forget(nonce string) error {
	path, err := s.path(nonce)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove seen record %s: %w", nonce, err)
	}
	return nil
}

// Prune removes records older than the retention window.
//
// Retention must exceed the submission TTL. A nonce whose record is dropped
// while the submission could still be considered fresh becomes replayable, so
// the two settings are coupled and Prune refuses a window that would break it.
func (s *FileSeenSet) Prune(retention, ttl time.Duration, now time.Time) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("seen-set is not configured")
	}
	if retention <= ttl {
		return 0, fmt.Errorf(
			"seen-set retention %s must exceed the submission time-to-live %s; "+
				"pruning sooner would make a still-fresh submission replayable", retention, ttl)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("read seen-set directory: %w", err)
	}
	pruned := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return pruned, fmt.Errorf("stat seen record %s: %w", entry.Name(), err)
		}
		if now.Sub(info.ModTime()) <= retention {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			return pruned, fmt.Errorf("prune seen record %s: %w", entry.Name(), err)
		}
		pruned++
	}
	return pruned, nil
}
