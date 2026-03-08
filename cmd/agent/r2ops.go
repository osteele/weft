package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// r2Get reads the content of an R2 key via rclone. Returns ("", nil) if the key doesn't exist.
func r2Get(bucket, key string) (string, error) {
	cmd := exec.Command("rclone", "cat", fmt.Sprintf("r2:%s/%s", bucket, key))
	out, err := cmd.Output()
	if err != nil {
		// rclone returns non-zero if key doesn't exist
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// r2Put writes content to an R2 key via rclone rcat.
func r2Put(bucket, key, content string) error {
	cmd := exec.Command("rclone", "rcat", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stdin = strings.NewReader(content)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// r2Delete removes an R2 key via rclone deletefile.
func r2Delete(bucket, key string) error {
	cmd := exec.Command("rclone", "deletefile", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
