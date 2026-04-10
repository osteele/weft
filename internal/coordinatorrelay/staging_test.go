package coordinatorrelay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildStageRootIncludesMainAndExtraPaths(t *testing.T) {
	projectDir := t.TempDir()
	mainFile := filepath.Join(projectDir, "main.py")
	if err := os.WriteFile(mainFile, []byte("print('hello')\n"), 0644); err != nil {
		t.Fatalf("write main file: %v", err)
	}

	extraDir := filepath.Join(t.TempDir(), "dataset")
	if err := os.MkdirAll(extraDir, 0755); err != nil {
		t.Fatalf("mkdir extra dir: %v", err)
	}
	extraDirFile := filepath.Join(extraDir, "train.txt")
	if err := os.WriteFile(extraDirFile, []byte("sample\n"), 0644); err != nil {
		t.Fatalf("write extra dir file: %v", err)
	}

	extraFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(extraFile, []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	cfg := []byte("[sync]\nextra_paths = [\"" + extraDir + "\"]\n")
	if err := os.WriteFile(filepath.Join(projectDir, ".weft.toml"), cfg, 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	root, entries, err := buildStageRoot(projectDir, "/remote/project", []string{extraFile})
	if err != nil {
		t.Fatalf("buildStageRoot: %v", err)
	}
	defer os.RemoveAll(root)

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	mainEntry := entries[0]
	if mainEntry.Kind != SourceEntryDir {
		t.Fatalf("main entry kind = %q, want %q", mainEntry.Kind, SourceEntryDir)
	}
	if mainEntry.RemotePath != "/remote/project" {
		t.Fatalf("main entry remote path = %q, want %q", mainEntry.RemotePath, "/remote/project")
	}
	if _, err := os.Stat(filepath.Join(root, mainEntry.RelPath, "main.py")); err != nil {
		t.Fatalf("main snapshot missing staged file: %v", err)
	}

	foundExtraFile := false
	foundExtraDir := false
	for _, entry := range entries[1:] {
		switch entry.RemotePath {
		case extraFile:
			foundExtraFile = true
			if entry.Kind != SourceEntryFile {
				t.Fatalf("extra file entry kind = %q, want %q", entry.Kind, SourceEntryFile)
			}
			if _, err := os.Stat(filepath.Join(root, entry.RelPath)); err != nil {
				t.Fatalf("staged extra file missing: %v", err)
			}
		case extraDir:
			foundExtraDir = true
			if entry.Kind != SourceEntryDir {
				t.Fatalf("extra dir entry kind = %q, want %q", entry.Kind, SourceEntryDir)
			}
			if _, err := os.Stat(filepath.Join(root, entry.RelPath, "train.txt")); err != nil {
				t.Fatalf("staged extra dir missing content: %v", err)
			}
		}
	}
	if !foundExtraFile {
		t.Fatalf("extra file entry not found")
	}
	if !foundExtraDir {
		t.Fatalf("extra dir entry not found")
	}
}

func TestBuildStageRootIncludesDeclaredLocalInputsInMainSnapshot(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "data", "conllu"), 0o755); err != nil {
		t.Fatalf("mkdir data/conllu: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "data", "conllu", "train.conllu"), []byte("1\ttest\n"), 0o644); err != nil {
		t.Fatalf("write train.conllu: %v", err)
	}

	root, entries, err := buildStageRoot(projectDir, "/remote/project", []string{"local:data/conllu/"})
	if err != nil {
		t.Fatalf("buildStageRoot: %v", err)
	}
	defer os.RemoveAll(root)

	if len(entries) == 0 {
		t.Fatalf("expected at least main source entry")
	}
	mainEntry := entries[0]
	if mainEntry.Kind != SourceEntryDir {
		t.Fatalf("main entry kind = %q, want %q", mainEntry.Kind, SourceEntryDir)
	}
	if _, err := os.Stat(filepath.Join(root, mainEntry.RelPath, "data", "conllu", "train.conllu")); err != nil {
		t.Fatalf("main snapshot missing declared local input: %v", err)
	}
}
