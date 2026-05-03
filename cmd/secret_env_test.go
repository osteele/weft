package cmd

import (
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/secrets"
)

func useCommandSecretStore(t *testing.T) {
	t.Helper()
	t.Setenv("WEFT_SECRET_BACKEND", "file")
	t.Setenv("WEFT_SECRET_STORE", filepath.Join(t.TempDir(), "secrets.json"))
}

func TestApplySecretEnvAutoHFTokenForHFInputs(t *testing.T) {
	useCommandSecretStore(t)
	if err := secrets.Set("hf", "token-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := applySecretEnv(nil, []string{"hf:meta-llama/Llama-3-8B"}, false, "", nil)
	if err != nil {
		t.Fatalf("applySecretEnv: %v", err)
	}
	if len(got) != 1 || got[0] != "HF_TOKEN=secret:hf" {
		t.Fatalf("env = %v, want HF_TOKEN=secret:hf", got)
	}
}

func TestApplySecretEnvHFTokenFromEnvStoresReference(t *testing.T) {
	useCommandSecretStore(t)
	t.Setenv("LOCAL_HF_TOKEN", "token-from-env")
	got, err := applySecretEnv(nil, nil, false, "env:LOCAL_HF_TOKEN", nil)
	if err != nil {
		t.Fatalf("applySecretEnv: %v", err)
	}
	if len(got) != 1 || got[0] != "HF_TOKEN=secret:hf" {
		t.Fatalf("env = %v, want HF_TOKEN=secret:hf", got)
	}
	value, err := secrets.Get("hf")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if value != "token-from-env" {
		t.Fatalf("stored value = %q, want token-from-env", value)
	}
}

func TestFormatEnvVarsForDisplayRedactsSecrets(t *testing.T) {
	got := formatEnvVarsForDisplay([]string{"HF_TOKEN=secret:hf", "FOO=bar"})
	want := "HF_TOKEN=<redacted>, FOO=bar"
	if got != want {
		t.Fatalf("display = %q, want %q", got, want)
	}
}
