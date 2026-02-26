package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte(`
# comment
FOO=bar
BAZ="quoted value"
SINGLE='single quoted'
EMPTY=
`), 0644)

	vars, err := LoadDotenv(envFile)
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"FOO=bar",
		"BAZ=quoted value",
		"SINGLE=single quoted",
		"EMPTY=",
	}
	for _, e := range expected {
		if !slices.Contains(vars, e) {
			t.Errorf("expected %q in vars, got %v", e, vars)
		}
	}
}

func TestLoadDotenv_Missing(t *testing.T) {
	vars, err := LoadDotenv("/nonexistent/.env")
	if err != nil {
		t.Fatal(err)
	}
	if vars != nil {
		t.Errorf("expected nil for missing file, got %v", vars)
	}
}

func TestLoadDotenvFiles_Merge(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\nB=2\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".env.local"), []byte("B=override\nC=3\n"), 0644)

	vars, err := LoadDotenvFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Build map for easier checking
	m := make(map[string]string)
	for _, v := range vars {
		k, val, _ := splitKV(v)
		m[k] = val
	}

	if m["A"] != "1" {
		t.Errorf("A: got %q, want 1", m["A"])
	}
	if m["B"] != "override" {
		t.Errorf("B: got %q, want override", m["B"])
	}
	if m["C"] != "3" {
		t.Errorf("C: got %q, want 3", m["C"])
	}
}

func splitKV(s string) (string, string, bool) {
	for i, c := range s {
		if c == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
