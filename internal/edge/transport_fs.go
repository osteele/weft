package edge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FSTransport is a Transport backed by a directory tree instead of a bucket.
//
// It exists so the protocol, its refusal paths, and the wait-side latency
// behavior are testable with no network and no bucket. It is a real transport,
// not a mock: verification runs against it exactly as it does against R2.
type FSTransport struct {
	root string
}

func NewFSTransport(root string) (*FSTransport, error) {
	if root == "" {
		return nil, fmt.Errorf("filesystem transport requires a root directory")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create transport root %s: %w", root, err)
	}
	return &FSTransport{root: filepath.Clean(root)}, nil
}

func (t *FSTransport) Name() string { return "filesystem:" + t.root }

// path maps an object key to a file, refusing any key that would escape the
// root once cleaned.
func (t *FSTransport) path(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("empty object key")
	}
	full := filepath.Clean(filepath.Join(t.root, filepath.FromSlash(key)))
	if full != t.root && !strings.HasPrefix(full, t.root+string(filepath.Separator)) {
		return "", fmt.Errorf("object key %q escapes the transport root", key)
	}
	return full, nil
}

func (t *FSTransport) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := t.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return data, nil
}

func (t *FSTransport) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := t.path(prefix)
	if err != nil {
		return nil, err
	}
	var keys []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(t.root, path)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	sort.Strings(keys)
	return keys, nil
}

func (t *FSTransport) Put(ctx context.Context, key string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := t.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", key, err)
	}
	// Write to a temporary file and rename, so a reader never observes a
	// partially written object.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".put-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", key, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", key, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("commit %s: %w", key, err)
	}
	return nil
}

// PutIfAbsent uses O_EXCL, the filesystem's equivalent of a conditional write,
// so the commit point is genuinely atomic here too rather than a check
// followed by a write.
func (t *FSTransport) PutIfAbsent(ctx context.Context, key string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := t.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", key, err)
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", key, ErrAlreadyExists)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", key, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(full)
		return fmt.Errorf("write %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(full)
		return fmt.Errorf("close %s: %w", key, err)
	}
	return nil
}

func (t *FSTransport) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := t.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
