package edge

import (
	"bytes"
	"context"
	"fmt"

	"github.com/osteele/weft/internal/r2"
)

// R2Transport is a Transport backed by a Cloudflare R2 bucket.
//
// The bucket is deliberately not the results bucket. R2 API tokens are scoped
// per bucket rather than per prefix, so a separate bucket is the only real
// isolation boundary available: an edge holds write credentials for inbound
// submissions and none for results, and a compromised edge therefore cannot
// rewrite artifacts. See decision 0026.
type R2Transport struct {
	client *r2.Client
}

func NewR2Transport(client *r2.Client) (*R2Transport, error) {
	if client == nil || !client.IsConfigured() {
		return nil, fmt.Errorf("edge R2 transport requires a configured inbound bucket")
	}
	return &R2Transport{client: client}, nil
}

func (t *R2Transport) Name() string { return "r2:" + t.client.Bucket() }

func (t *R2Transport) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := t.client.GetObject(ctx, key)
	if err != nil {
		// Distinguish confirmed absence from an unreachable store. Callers act
		// on absence, so collapsing the two would let a network failure read as
		// "the object is not there".
		if r2.IsNotFound(err) {
			return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return data, nil
}

func (t *R2Transport) List(ctx context.Context, prefix string) ([]string, error) {
	objects, err := t.client.ListObjects(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	keys := make([]string, 0, len(objects))
	for _, obj := range objects {
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (t *R2Transport) Put(ctx context.Context, key string, body []byte) error {
	if err := t.client.PutObject(ctx, key, bytes.NewReader(body), "application/octet-stream"); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// PutIfAbsent is the commit point for a submission. If-None-Match makes the
// first writer win, so a retried submission that already committed fails the
// precondition rather than creating a second job.
func (t *R2Transport) PutIfAbsent(ctx context.Context, key string, body []byte) error {
	_, err := t.client.PutObjectConditional(ctx, key, bytes.NewReader(body), "application/octet-stream", "", "*")
	if err != nil {
		if r2.IsPreconditionFailed(err) {
			return fmt.Errorf("%s: %w", key, ErrAlreadyExists)
		}
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (t *R2Transport) Delete(ctx context.Context, key string) error {
	if err := t.client.DeleteObject(ctx, key); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
