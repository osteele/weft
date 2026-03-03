package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDependencies_Empty(t *testing.T) {
	result := CheckDependencies("", nil, t.TempDir())
	if result.Result != DepOK {
		t.Errorf("expected OK for empty deps, got %v", result.Result)
	}
}

func TestCheckDependencies_Satisfied(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("0\n"), 0644)

	result := CheckDependencies("42", nil, dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_Waiting(t *testing.T) {
	dir := t.TempDir()

	result := CheckDependencies("42", nil, dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}

func TestCheckDependencies_Failed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("1\n"), 0644)

	result := CheckDependencies("42", nil, dir)
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
	result := CheckDependencies("42:any", nil, dir)
	if result.Result != DepOK {
		t.Errorf("expected OK with :any mode, got %v", result.Result)
	}
}

func TestCheckDependencies_Multiple(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "10.status"), []byte("0\n"), 0644)
	os.WriteFile(filepath.Join(dir, "20.status"), []byte("0\n"), 0644)

	result := CheckDependencies("10,20", nil, dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_OneWaiting(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "10.status"), []byte("0\n"), 0644)
	// 20 is not completed

	result := CheckDependencies("10,20", nil, dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}

func TestCheckDependencies_ArtifactSatisfied(t *testing.T) {
	dir := t.TempDir()
	// Write a satisfied file for artifact "output/model.pt" version 100
	satisfiedPath := ArtifactSatisfiedFile(dir, "output/model.pt", 100)
	os.WriteFile(satisfiedPath, []byte("0\n"), 0644)

	result := CheckDependencies("", []string{"output/model.pt:100"}, dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_ArtifactWaiting(t *testing.T) {
	dir := t.TempDir()
	// No satisfied file exists

	result := CheckDependencies("", []string{"output/model.pt:100"}, dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}

func TestCheckDependencies_ArtifactFailed(t *testing.T) {
	dir := t.TempDir()
	// Write a satisfied file with non-zero exit code
	satisfiedPath := ArtifactSatisfiedFile(dir, "output/model.pt", 100)
	os.WriteFile(satisfiedPath, []byte("1\n"), 0644)

	result := CheckDependencies("", []string{"output/model.pt:100"}, dir)
	if result.Result != DepFailed {
		t.Errorf("expected Failed, got %v", result.Result)
	}
}

func TestCheckDependencies_BothJobAndArtifact(t *testing.T) {
	dir := t.TempDir()
	// Job dep satisfied
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("0\n"), 0644)
	// Artifact dep satisfied
	satisfiedPath := ArtifactSatisfiedFile(dir, "output/model.pt", 100)
	os.WriteFile(satisfiedPath, []byte("0\n"), 0644)

	result := CheckDependencies("42", []string{"output/model.pt:100"}, dir)
	if result.Result != DepOK {
		t.Errorf("expected OK, got %v", result.Result)
	}
}

func TestCheckDependencies_JobOKArtifactWaiting(t *testing.T) {
	dir := t.TempDir()
	// Job dep satisfied
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("0\n"), 0644)
	// Artifact dep NOT satisfied

	result := CheckDependencies("42", []string{"output/model.pt:100"}, dir)
	if result.Result != DepWaiting {
		t.Errorf("expected Waiting, got %v", result.Result)
	}
}
