package artifacts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataplane"
)

func TestValidatePayloadName(t *testing.T) {
	for _, name := range []string{"config", "model.json", "a_b-1"} {
		if err := ValidatePayloadName(name); err != nil {
			t.Errorf("ValidatePayloadName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "two words", "line\nfeed", "config;rm", "$(command)", "'quoted'", strings.Repeat("a", 129)} {
		if err := ValidatePayloadName(name); err == nil {
			t.Errorf("ValidatePayloadName(%q) succeeded", name)
		}
	}
}

func TestCapturePayloadIsIndependentOfSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(source, []byte("admitted bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	stored, size, digest, err := CapturePayload(source)
	if err != nil {
		t.Fatalf("CapturePayload: %v", err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("admitted bytes")))
	if size != 14 || digest != wantDigest {
		t.Fatalf("capture metadata = size %d digest %s", size, digest)
	}
	if err := os.WriteFile(source, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	local, err := LocalPathFromStored(stored)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "admitted bytes" {
		t.Fatalf("stored bytes = %q", got)
	}
}

func TestStagePayloadsVerifiesBytesAndUsesPrivateModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	payload := dataplane.JobPayload{Name: "token", SizeBytes: 6, SHA256: "bef57ec7f53a6d40beb640a780a639c83bc29ac8a9816f1fc6c5c6dcd93c4721", R2Key: "assets/bef57"}
	dir, err := StagePayloads(42, []dataplane.JobPayload{payload}, func(key, dest string) error {
		if key != payload.R2Key {
			t.Fatalf("key = %q", key)
		}
		return os.WriteFile(dest, []byte("abcdef"), 0o644)
	})
	if err != nil {
		t.Fatalf("StagePayloads: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join(".cache", "weft", "payloads", "42")) {
		t.Fatalf("dir = %q", dir)
	}
	dirInfo, _ := os.Stat(dir)
	fileInfo, _ := os.Stat(filepath.Join(dir, "token"))
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o", got)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o", got)
	}
}
