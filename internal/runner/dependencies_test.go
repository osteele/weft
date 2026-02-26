package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDependencies_Empty(t *testing.T) {
	result := CheckDependencies("", t.TempDir())
	if result.Result != DepOK {
		t.Errorf("expected OK for empty deps, got %v", result.Result)
	}
}

func TestCheckDependencies_Satisfied(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("0\n"), 0644)

	result := CheckDependencies("42", dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_Waiting(t *testing.T) {
	dir := t.TempDir()

	result := CheckDependencies("42", dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}

func TestCheckDependencies_Failed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("1\n"), 0644)

	result := CheckDependencies("42", dir)
	if result.Result != DepFailed {
		t.Errorf("expected Failed, got %v", result.Result)
	}
	if result.FailedDep != "42:1" {
		t.Errorf("expected '42:1', got %q", result.FailedDep)
	}
}

func TestCheckDependencies_AnyMode(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("1\n"), 0644)

	// "any" mode accepts any exit code
	result := CheckDependencies("42:any", dir)
	if result.Result != DepOK {
		t.Errorf("expected OK with :any mode, got %v", result.Result)
	}
}

func TestCheckDependencies_Multiple(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "10.status"), []byte("0\n"), 0644)
	os.WriteFile(filepath.Join(dir, "20.status"), []byte("0\n"), 0644)

	result := CheckDependencies("10,20", dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_OneWaiting(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "10.status"), []byte("0\n"), 0644)
	// 20 is not completed

	result := CheckDependencies("10,20", dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}
