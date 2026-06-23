package dataloc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHFDirName(t *testing.T) {
	tests := []struct {
		name   string
		want   DataAsset
		wantOK bool
	}{
		{"models--meta-llama--Llama-3-8B", DataAsset{AssetHFModel, "meta-llama/Llama-3-8B"}, true},
		{"models--bert-base-uncased", DataAsset{AssetHFModel, "bert-base-uncased"}, true},
		{"models--google--gemma-2b", DataAsset{AssetHFModel, "google/gemma-2b"}, true},
		{"datasets--wikitext", DataAsset{AssetHFDataset, "wikitext"}, true},
		{"datasets--allenai--c4", DataAsset{AssetHFDataset, "allenai/c4"}, true},
		{"datasets--HuggingFaceFW--fineweb", DataAsset{AssetHFDataset, "HuggingFaceFW/fineweb"}, true},
		// Invalid
		{"snapshots", DataAsset{}, false},
		{"", DataAsset{}, false},
		{"refs", DataAsset{}, false},
		{".locks", DataAsset{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseHFDirName(tt.name)
			if ok != tt.wantOK {
				t.Errorf("ParseHFDirName(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
				return
			}
			if ok && (got.Kind != tt.want.Kind || got.ID != tt.want.ID) {
				t.Errorf("ParseHFDirName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestHFCacheScanCommand_DoesNotRunDuWhenIncompleteListIsEmpty(t *testing.T) {
	cmd := hfCacheScanCommand()
	if strings.Contains(cmd, "xargs -0 du") {
		t.Fatalf("scan command uses xargs for incomplete blobs; empty matches make du measure the current directory:\n%s", cmd)
	}
	for _, want := range []string{
		"find \"$_d\" -name '*.incomplete' -type f -exec du -sb {} +",
		"find \"$_d\" -name '*.incomplete' -type f -exec du -sk {} +",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("scan command missing safe incomplete-size fragment %q:\n%s", want, cmd)
		}
	}
}

// TestHFCacheScanCommand_ClassifiesUnreadableEntry is a regression test for the
// cool30 failure where a shared NAS HF cache entry owned by another user
// (root:root, mode 770, created by a root-context container download) was
// misreported as "no-snapshots". The scanner needs search permission on the
// entry dir to stat snapshots/; without it, the snapshot is invisible and the
// entry must be classified "unreadable", not "no-snapshots".
func TestHFCacheScanCommand_ClassifiesUnreadableEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits")
	}
	hub := filepath.Join(t.TempDir(), "hub")

	// A normal, readable entry (control) and one the user cannot search into.
	for _, name := range []string{"datasets--readable--ok", "datasets--locked--ent"} {
		snap := filepath.Join(hub, name, "snapshots", "rev")
		if err := os.MkdirAll(snap, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snap, "README.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(hub, "datasets--locked--ent")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // restore so TempDir cleanup can recurse

	cmd := exec.Command("sh", "-c", hfCacheScanCommand())
	// HF_HUB_CACHE wins over HF_HOME/$HOME in ResolveHFCacheDirShellVar.
	cmd.Env = append(os.Environ(), "HF_HUB_CACHE="+hub)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("scan command failed: %v", err)
	}
	byID := map[string]string{}
	for _, r := range parseHFCacheDetailedOutput(string(out)) {
		byID[r.Asset.ID] = r.Status
	}
	if got := byID["readable/ok"]; got != "ok" {
		t.Errorf("readable entry status = %q, want ok", got)
	}
	if got := byID["locked/ent"]; got != "unreadable" {
		t.Errorf("unreadable entry status = %q, want unreadable", got)
	}
}

func TestHFCacheScanCommand_AllowsValidEntryWithNestedDebris(t *testing.T) {
	hub := filepath.Join(t.TempDir(), "hub")
	entry := filepath.Join(hub, "models--meta-llama--Llama-3.2-3B")
	for _, dir := range []string{
		filepath.Join(entry, "snapshots", "rev"),
		filepath.Join(entry, "blobs"),
		filepath.Join(entry, "models--meta-llama--Llama-3.2-3B", "snapshots", "rev"),
		filepath.Join(entry, "models--meta-llama--Llama-3.2-3B", "blobs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(entry, "blobs", "top"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "models--meta-llama--Llama-3.2-3B", "blobs", "nested.incomplete"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", hfCacheScanCommand())
	cmd.Env = append(os.Environ(), "HF_HUB_CACHE="+hub)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("scan command failed: %v", err)
	}
	results := parseHFCacheDetailedOutput(string(out))
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %s", len(results), out)
	}
	if got := results[0].Status; got != "ok" {
		t.Fatalf("status = %q, want ok for valid top-level cache with stale nested debris; output:\n%s", got, out)
	}
}

func TestHfDirToID(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"meta-llama--Llama-3-8B", "meta-llama/Llama-3-8B"},
		{"bert-base-uncased", "bert-base-uncased"},
		{"google--gemma-2b", "google/gemma-2b"},
		{"wikitext", "wikitext"},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			got := hfDirToID(tt.dir)
			if got != tt.want {
				t.Errorf("hfDirToID(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestParseHFCacheDetailedOutput(t *testing.T) {
	output := "16284561408\tok\t/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B\n" +
		"5500000000\tincomplete\t/home/user/.cache/huggingface/hub/models--EleutherAI--pythia-1.4b\n" +
		"30000000000\tnested\t/home/user/.cache/huggingface/hub/models--EleutherAI--pythia-410m\n" +
		"0\tno-snapshots\t/home/user/.cache/huggingface/hub/models--broken\n" +
		"123\tnested,incomplete\t/home/user/.cache/huggingface/hub/datasets--bad\n" +
		"1048576000\tok\t/home/user/.cache/huggingface/hub/datasets--wikitext\n"

	results := parseHFCacheDetailedOutput(output)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}

	cases := []struct {
		i      int
		kind   AssetKind
		id     string
		size   int64
		status string
	}{
		{0, AssetHFModel, "meta-llama/Llama-3-8B", 16284561408, "ok"},
		{1, AssetHFModel, "EleutherAI/pythia-1.4b", 5500000000, "incomplete"},
		{2, AssetHFModel, "EleutherAI/pythia-410m", 30000000000, "nested"},
		{3, AssetHFModel, "broken", 0, "no-snapshots"},
		{4, AssetHFDataset, "bad", 123, "nested,incomplete"},
		{5, AssetHFDataset, "wikitext", 1048576000, "ok"},
	}
	for _, c := range cases {
		r := results[c.i]
		if r.Asset.Kind != c.kind || r.Asset.ID != c.id {
			t.Errorf("result[%d] asset: got %v, want %s/%s", c.i, r.Asset, c.kind, c.id)
		}
		if r.SizeBytes != c.size {
			t.Errorf("result[%d] size: got %d, want %d", c.i, r.SizeBytes, c.size)
		}
		if r.Status != c.status {
			t.Errorf("result[%d] status: got %q, want %q", c.i, r.Status, c.status)
		}
	}
}

func TestParseHFCacheDetailedOutput_Empty(t *testing.T) {
	if got := parseHFCacheDetailedOutput(""); len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestParseHFCacheDetailedOutput_SkipsBlankAndMalformedLines(t *testing.T) {
	output := "\n" +
		"\r\n" +
		"   \n" +
		"not enough columns\n" +
		"123\tok\n" + // only 2 fields
		"123\tok\t/home/user/.cache/huggingface/hub/snapshots\n" + // not an HF dir
		"42\tok\t/home/user/.cache/huggingface/hub/models--keep-me\n"

	results := parseHFCacheDetailedOutput(output)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Asset.ID != "keep-me" {
		t.Errorf("got %q, want keep-me", results[0].Asset.ID)
	}
}
