package dataloc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/agentenv"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ssh"
)

// RemoteR2Credentials is the JSON credentials blob piped to
// `weft r2 put-content --creds-stdin` over the SSH connection's stdin. Passing
// secrets on stdin (rather than as command-line env assignments) keeps them out
// of the remote process's argv/environment, which is readable via `ps` by any
// user on the holder host during the upload window.
type RemoteR2Credentials struct {
	AccountID       string `json:"account_id"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Bucket          string `json:"bucket"`
}

// hostStdinCommandRunner runs a remote command with data piped to its stdin.
// Injectable for tests.
var hostStdinCommandRunner = func(host, command, stdin string) (string, string, error) {
	return ssh.RunWithStdin(host, command, stdin)
}

const (
	RemoteR2AccountIDEnv       = "WEFT_R2_ACCOUNT_ID"
	RemoteR2AccessKeyIDEnv     = "WEFT_R2_ACCESS_KEY_ID"
	RemoteR2SecretAccessKeyEnv = "WEFT_R2_SECRET_ACCESS_KEY"
	RemoteR2BucketEnv          = "WEFT_R2_BUCKET"
)

// remoteWeftBinary is the binary name invoked over SSH on a holder host. On-prem
// hosts run the agent build (`go build -o weft-agent ./cmd/agent`, deployed to
// ~/.cache/weft/bin by `weft queue update`); there is no full `weft` binary
// there, so host-side digest/upload must call the agent, which exposes matching
// `r2 content-info` / `r2 put-content` subcommands.
const remoteWeftBinary = "weft-agent"

// DigestPathOnHost computes the same content metadata as DigestPath, but on
// the holder host so remote publishes do not first copy the bytes locally.
func DigestPathOnHost(ctx context.Context, host, path string) (ContentInfo, error) {
	if strings.TrimSpace(host) == "" {
		return ContentInfo{}, fmt.Errorf("host is required")
	}
	if strings.TrimSpace(path) == "" {
		return ContentInfo{}, fmt.Errorf("path is required")
	}
	cmd := agentenv.ShellPrefix(remoteWeftBinary + " r2 content-info --path " + shellQuote(path))
	stdout, stderr, err := hostCommandRunner(ctx, host, cmd)
	if err != nil {
		return ContentInfo{}, formatRemoteR2CommandError(host, "digest content", stderr, err)
	}
	var info ContentInfo
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return ContentInfo{}, fmt.Errorf("parse content-info output from %s: %w", host, err)
	}
	if info.Hash == "" {
		return ContentInfo{}, fmt.Errorf("content-info output from %s did not include a hash", host)
	}
	return info, nil
}

type R2PutFromHostRequest struct {
	Path        string
	Key         string
	ContentType ContentType
	R2          cloud.R2Config
}

// R2PutFromHost runs the upload on the holder host. R2 credentials are piped to
// the remote `weft r2 put-content` process on stdin, never placed in its argv or
// environment, and never written to a file on the host.
func R2PutFromHost(_ context.Context, host string, req R2PutFromHostRequest) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host is required")
	}
	if strings.TrimSpace(req.Path) == "" {
		return fmt.Errorf("path is required")
	}
	if strings.TrimSpace(req.Key) == "" {
		return fmt.Errorf("R2 key is required")
	}
	if req.R2.Bucket == "" || req.R2.AccountID == "" || req.R2.AccessKeyID == "" || req.R2.SecretAccessKey == "" {
		return fmt.Errorf("R2 credentials are incomplete")
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = ContentTypeFile
	}
	credsJSON, err := json.Marshal(RemoteR2Credentials{
		AccountID:       req.R2.AccountID,
		AccessKeyID:     req.R2.AccessKeyID,
		SecretAccessKey: req.R2.SecretAccessKey,
		Bucket:          req.R2.Bucket,
	})
	if err != nil {
		return fmt.Errorf("encode R2 credentials: %w", err)
	}
	command := buildR2PutFromHostCommand(req.Path, req.Key, contentType)
	_, stderr, err := hostStdinCommandRunner(host, command, string(credsJSON))
	if err != nil {
		return formatRemoteR2CommandError(host, "upload content to R2", stderr, err)
	}
	return nil
}

func buildR2PutFromHostCommand(path, key string, contentType ContentType) string {
	cmd := fmt.Sprintf("%s r2 put-content --key %s --path %s --content-type %s --creds-stdin",
		remoteWeftBinary, shellQuote(key), shellQuote(path), shellQuote(string(contentType)))
	return agentenv.ShellPrefix(cmd)
}

func formatRemoteR2CommandError(host, action, stderr string, err error) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("%s on %s: %s: %w", action, host, stderr, err)
	}
	return fmt.Errorf("%s on %s: %w", action, host, err)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
