package dataloc

import "testing"

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
