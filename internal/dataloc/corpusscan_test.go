package dataloc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpusScanCommand_MissingBaseDirIsError guards the regression where a
// transiently absent/unmounted corpus base dir produced exit 0 with empty output,
// indistinguishable from "present but no corpora" — the caller then pruned every
// corpus row, and because corpus inputs are non-transportable the affected jobs
// became permanently unplaceable. A missing base dir must exit non-zero with the
// sentinel so the caller skips the prune.
func TestCorpusScanCommand_MissingBaseDirIsError(t *testing.T) {
	home := t.TempDir() // no .local/share/corpora under it
	cmd := exec.Command("sh", "-c", corpusScanCommand())
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected non-zero exit for missing corpus base dir; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), scanBaseDirSentinel) {
		t.Fatalf("stderr = %q, want base-dir sentinel %q", stderr.String(), scanBaseDirSentinel)
	}
}

// TestCorpusScanCommand_PresentButEmptyExitsZero confirms the safe case still
// works: a present base dir with no corpora exits 0 with empty output, so the
// caller may prune stale rows (the corpora are positively confirmed gone).
func TestCorpusScanCommand_PresentButEmptyExitsZero(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, CorpusBaseDir), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", corpusScanCommand())
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("present-but-empty corpus dir should exit 0: %v (stderr=%q)", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("stdout = %q, want empty output for a present-but-empty corpus dir", stdout.String())
	}
}

// TestScanCorpusDir_BaseDirUnavailableMapsToSentinelError verifies the sentinel
// on stderr is mapped to ErrScanBaseDirUnavailable so callers can skip pruning.
func TestScanCorpusDir_BaseDirUnavailableMapsToSentinelError(t *testing.T) {
	orig := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = orig })
	hostCommandRunner = func(_ context.Context, _ string, _ string) (string, string, error) {
		return "", scanBaseDirSentinel + " /home/u/.local/share/corpora", fmt.Errorf("exit status 3")
	}
	if _, err := ScanCorpusDir("host-x"); !errors.Is(err, ErrScanBaseDirUnavailable) {
		t.Fatalf("err = %v, want ErrScanBaseDirUnavailable", err)
	}
}

// TestScanCorpusDir_GenericErrorNotMisclassified ensures an unrelated failure
// (e.g. network) is NOT reported as base-dir-unavailable — that would let a
// real connectivity failure masquerade as a clean signal.
func TestScanCorpusDir_GenericErrorNotMisclassified(t *testing.T) {
	orig := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = orig })
	netErr := fmt.Errorf("ssh: connect: connection refused")
	hostCommandRunner = func(_ context.Context, _ string, _ string) (string, string, error) {
		return "", "", netErr
	}
	_, err := ScanCorpusDir("host-x")
	if errors.Is(err, ErrScanBaseDirUnavailable) {
		t.Fatalf("generic error must not map to ErrScanBaseDirUnavailable: %v", err)
	}
	if !errors.Is(err, netErr) {
		t.Fatalf("generic error should be preserved: %v", err)
	}
}

func TestParseCorpusScanOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []HostDataEntry
	}{
		{
			name: "du -sb output",
			output: "1048576\t/home/user/.local/share/corpora/penn-treebank/conllu\n" +
				"2097152\t/home/user/.local/share/corpora/universal-dependencies/en_ewt\n",
			want: []HostDataEntry{
				{Host: "testhost", Asset: DataAsset{AssetCorpus, "penn-treebank/conllu"}, Path: "/home/user/.local/share/corpora/penn-treebank/conllu", SizeBytes: 1048576},
				{Host: "testhost", Asset: DataAsset{AssetCorpus, "universal-dependencies/en_ewt"}, Path: "/home/user/.local/share/corpora/universal-dependencies/en_ewt", SizeBytes: 2097152},
			},
		},
		{
			name:   "plain path output",
			output: "/home/user/.local/share/corpora/penn-treebank/conllu\n",
			want: []HostDataEntry{
				{Host: "testhost", Asset: DataAsset{AssetCorpus, "penn-treebank/conllu"}, Path: "/home/user/.local/share/corpora/penn-treebank/conllu"},
			},
		},
		{
			name:   "trailing slash stripped",
			output: "/home/user/.local/share/corpora/penn-treebank/conllu/\n",
			want: []HostDataEntry{
				{Host: "testhost", Asset: DataAsset{AssetCorpus, "penn-treebank/conllu"}, Path: "/home/user/.local/share/corpora/penn-treebank/conllu"},
			},
		},
		{
			name:   "empty output",
			output: "",
			want:   nil,
		},
		{
			name:   "path too shallow (no subset)",
			output: "/home/user/.local/share/corpora/penn-treebank\n",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCorpusScanOutput(tt.output, "testhost")
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Host != tt.want[i].Host {
					t.Errorf("[%d] Host: got %q, want %q", i, got[i].Host, tt.want[i].Host)
				}
				if got[i].Asset != tt.want[i].Asset {
					t.Errorf("[%d] Asset: got %v, want %v", i, got[i].Asset, tt.want[i].Asset)
				}
				if got[i].Path != tt.want[i].Path {
					t.Errorf("[%d] Path: got %q, want %q", i, got[i].Path, tt.want[i].Path)
				}
				if got[i].SizeBytes != tt.want[i].SizeBytes {
					t.Errorf("[%d] SizeBytes: got %d, want %d", i, got[i].SizeBytes, tt.want[i].SizeBytes)
				}
			}
		})
	}
}

func TestExtractCorpusID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/home/user/.local/share/corpora/penn-treebank/conllu", "penn-treebank/conllu"},
		{"/home/user/.local/share/corpora/universal-dependencies/en_ewt", "universal-dependencies/en_ewt"},
		{"/home/user/.local/share/corpora/penn-treebank", ""},
		{"/no/corpora/here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractCorpusID(tt.path)
			if got != tt.want {
				t.Errorf("extractCorpusID(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
