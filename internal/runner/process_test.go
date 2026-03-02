package runner

import (
	"strings"
	"testing"
)

// assertEnvVar checks that result contains exactly one entry for key with the given value.
func assertEnvVar(t *testing.T, result []string, key, wantVal string) {
	t.Helper()
	prefix := key + "="
	count := 0
	var gotVal string
	for _, ev := range result {
		if strings.HasPrefix(ev, prefix) {
			count++
			gotVal = strings.TrimPrefix(ev, prefix)
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 %s, got %d", key, count)
	}
	if gotVal != wantVal {
		t.Errorf("expected %s=%s, got %s=%s", key, wantVal, key, gotVal)
	}
}

func TestMergeEnvVars_LastWriterWins(t *testing.T) {
	base := []string{
		"HOME=/home/user",
		"PATH=/usr/bin",
		"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7,8,9",
	}
	overlay := []string{
		"MY_VAR=hello",
		"CUDA_VISIBLE_DEVICES=0",
	}

	result := mergeEnvVars(base, overlay)

	assertEnvVar(t, result, "CUDA_VISIBLE_DEVICES", "0")
	assertEnvVar(t, result, "HOME", "/home/user")
	assertEnvVar(t, result, "PATH", "/usr/bin")
	assertEnvVar(t, result, "MY_VAR", "hello")
}

func TestMergeEnvVars_OverlayDuplicates(t *testing.T) {
	// When overlay itself has duplicates, last one wins
	base := []string{"A=1"}
	overlay := []string{
		"CUDA_VISIBLE_DEVICES=7", // from dotenv
		"B=2",
		"CUDA_VISIBLE_DEVICES=0", // from GPU class resolution
	}

	result := mergeEnvVars(base, overlay)

	assertEnvVar(t, result, "CUDA_VISIBLE_DEVICES", "0")
}

func TestMergeEnvVars_NoOverlap(t *testing.T) {
	base := []string{"A=1", "B=2"}
	overlay := []string{"C=3", "D=4"}

	result := mergeEnvVars(base, overlay)
	if len(result) != 4 {
		t.Errorf("expected 4 vars, got %d: %v", len(result), result)
	}
}

func TestMergeEnvVars_EmptyOverlay(t *testing.T) {
	base := []string{"A=1", "B=2"}
	result := mergeEnvVars(base, nil)
	if len(result) != 2 {
		t.Errorf("expected 2 vars, got %d", len(result))
	}
}
