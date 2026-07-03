package dataloc

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/osteele/weft/internal/r2"
)

// PutContentToR2 uploads a file, or a directory as a deterministic tar.gz, to
// the content-addressed R2 key. Shared by the full CLI's `weft r2 put-content`
// and the agent's `weft-agent r2 put-content` so the host-side path and the
// local path use identical archive/upload logic.
func PutContentToR2(ctx context.Context, client *r2.Client, path string, contentType ContentType, key string) error {
	if contentType == ContentTypeDirectory {
		pr, pw := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			err := WriteDirectoryArchive(path, pw)
			_ = pw.CloseWithError(err)
			errCh <- err
		}()
		putErr := client.PutObject(ctx, key, pr, "application/gzip")
		_ = pr.Close()
		archiveErr := <-errCh
		if putErr != nil {
			return putErr
		}
		if archiveErr != nil {
			return fmt.Errorf("archive directory: %w", archiveErr)
		}
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return client.PutObject(ctx, key, f, "application/octet-stream")
}

// R2ClientFromCredentials builds an R2 client from a credentials blob (e.g. one
// piped to `weft-agent r2 put-content --creds-stdin`).
func R2ClientFromCredentials(creds RemoteR2Credentials) (*r2.Client, error) {
	if creds.AccountID == "" || creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.Bucket == "" {
		return nil, fmt.Errorf("incomplete R2 credentials")
	}
	return r2.New(r2.Config{
		AccountID:       creds.AccountID,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		Bucket:          creds.Bucket,
	})
}
