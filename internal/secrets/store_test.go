package secrets

import (
	"errors"
	"path/filepath"
	"testing"
)

func useTestStore(t *testing.T) {
	t.Helper()
	t.Setenv("WEFT_SECRET_BACKEND", "file")
	t.Setenv("WEFT_SECRET_STORE", filepath.Join(t.TempDir(), "secrets.json"))
}

func TestSetGetListRemove(t *testing.T) {
	useTestStore(t)

	if err := Set("hf", "token-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, err := Get("hf")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if value != "token-value" {
		t.Fatalf("value = %q, want token-value", value)
	}
	names, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "hf" {
		t.Fatalf("names = %v, want [hf]", names)
	}
	if err := Remove("hf"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Get("hf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after remove error = %v, want ErrNotFound", err)
	}
}

func TestResolveEnvVars(t *testing.T) {
	useTestStore(t)
	if err := Set("hf", "hf-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ResolveEnvVars([]string{"FOO=bar", "HF_TOKEN=secret:hf"})
	if err != nil {
		t.Fatalf("ResolveEnvVars: %v", err)
	}
	want := []string{"FOO=bar", "HF_TOKEN=hf-secret"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRedactEnvVars(t *testing.T) {
	got := RedactEnvVars([]string{"FOO=bar", "HF_TOKEN=secret:hf", "API_KEY=literal", "CUDA_VISIBLE_DEVICES=0"})
	want := []string{"FOO=bar", "HF_TOKEN=<redacted>", "API_KEY=<redacted>", "CUDA_VISIBLE_DEVICES=0"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
