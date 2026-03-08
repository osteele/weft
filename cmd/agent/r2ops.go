package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// r2Timeout is the maximum time for any single rclone R2 operation.
const r2Timeout = 15 * time.Second

// r2Get reads the content of an R2 key via rclone. Returns ("", nil) if the key doesn't exist.
func r2Get(bucket, key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "cat", fmt.Sprintf("r2:%s/%s", bucket, key))
	out, err := cmd.Output()
	if err != nil {
		// rclone returns non-zero if key doesn't exist or timeout
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// r2Put writes content to an R2 key via rclone rcat.
func r2Put(bucket, key, content string) error {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "rcat", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stdin = strings.NewReader(content)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// r2Delete removes an R2 key via rclone deletefile.
func r2Delete(bucket, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "deletefile", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
