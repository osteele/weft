package dataloc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestDigestPathOnHostRunsRemoteContentInfo(t *testing.T) {
	prev := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = prev })
	hostCommandRunner = func(_ context.Context, host, command string) (string, string, error) {
		if host != "cool30" {
			return "", "", fmt.Errorf("host = %s", host)
		}
		if !strings.Contains(command, "weft-agent r2 content-info") || !strings.Contains(command, "--path /remote/checkpoint") {
			return "", "", fmt.Errorf("command = %s", command)
		}
		out, _ := json.Marshal(ContentInfo{Hash: strings.Repeat("a", 64), SizeBytes: 123, ContentType: ContentTypeDirectory})
		return string(out), "", nil
	}

	info, err := DigestPathOnHost(context.Background(), "cool30", "/remote/checkpoint")
	if err != nil {
		t.Fatalf("DigestPathOnHost: %v", err)
	}
	if info.Hash != strings.Repeat("a", 64) || info.SizeBytes != 123 || info.ContentType != ContentTypeDirectory {
		t.Fatalf("info = %+v", info)
	}
}

func TestR2ClientFromCredentialsRejectsIncomplete(t *testing.T) {
	for _, creds := range []RemoteR2Credentials{
		{AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"},    // no account
		{AccountID: "a", SecretAccessKey: "s", Bucket: "b"},      // no access key
		{AccountID: "a", AccessKeyID: "k", Bucket: "b"},          // no secret
		{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s"}, // no bucket
	} {
		if _, err := R2ClientFromCredentials(creds); err == nil {
			t.Fatalf("incomplete credentials %+v accepted", creds)
		}
	}
}

func TestR2PutFromHostPipesCredentialsOnStdin(t *testing.T) {
	prev := hostStdinCommandRunner
	t.Cleanup(func() { hostStdinCommandRunner = prev })
	var gotStdin string
	hostStdinCommandRunner = func(host, command, stdin string) (string, string, error) {
		if host != "cool30" {
			return "", "", fmt.Errorf("host = %s", host)
		}
		gotStdin = stdin
		for _, want := range []string{
			"weft-agent r2 put-content",
			"--key assets/",
			"--path /remote/checkpoint",
			"--content-type directory",
			"--creds-stdin",
		} {
			if !strings.Contains(command, want) {
				return "", "", fmt.Errorf("command missing %q: %s", want, command)
			}
		}
		// Secrets must never appear in the command's argv/env: those are
		// readable via `ps` on the holder host during the upload.
		for _, leak := range []string{
			"acct-789", "akid-123", "sk-456", "bkt-xyz",
			RemoteR2AccountIDEnv, RemoteR2AccessKeyIDEnv,
			RemoteR2SecretAccessKeyEnv, RemoteR2BucketEnv,
		} {
			if strings.Contains(command, leak) {
				return "", "", fmt.Errorf("command leaks %q: %s", leak, command)
			}
		}
		if strings.Contains(command, "CopyFrom") || strings.Contains(command, "scp ") {
			return "", "", fmt.Errorf("command should not copy through local machine: %s", command)
		}
		return "", "", nil
	}

	err := R2PutFromHost(context.Background(), "cool30", R2PutFromHostRequest{
		Path:        "/remote/checkpoint",
		Key:         "assets/" + strings.Repeat("b", 64),
		ContentType: ContentTypeDirectory,
		R2: cloud.R2Config{
			AccountID:       "acct-789",
			AccessKeyID:     "akid-123",
			SecretAccessKey: "sk-456",
			Bucket:          "bkt-xyz",
		},
	})
	if err != nil {
		t.Fatalf("R2PutFromHost: %v", err)
	}
	var creds RemoteR2Credentials
	if err := json.Unmarshal([]byte(gotStdin), &creds); err != nil {
		t.Fatalf("stdin is not credentials JSON: %v (%q)", err, gotStdin)
	}
	if creds.AccountID != "acct-789" || creds.AccessKeyID != "akid-123" ||
		creds.SecretAccessKey != "sk-456" || creds.Bucket != "bkt-xyz" {
		t.Fatalf("stdin credentials = %+v", creds)
	}
}
