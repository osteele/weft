package dataloc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTorchRequirementCeiling(t *testing.T) {
	cases := []struct {
		req  string
		want string
	}{
		{"torch>=2.2,<2.7", "2.6"},
		{"torch>=2.2, <2.7 ; sys_platform == 'linux'", "2.6"},
		{"torch<2.7rc1", "2.6"},
		{"torch<2.7.1", "2.7"},
		{"torch<=2.6.3", "2.6"},
		{"torch~=2.6.0", "2.6"},
		{"torch==2.6.*", "2.6"},
		{"torch>=2.2,<2.9,<2.7", "2.6"},
		// No bound on the release line: the range can resolve the newest torch.
		{"torch>=2.2", ""},
		{"torch~=2.6", ""},
		{"torch>=2.2,<3", ""},
		{"torch==2.*", ""},
		{"torch!=2.5.0", ""},
	}
	for _, c := range cases {
		got := parseTorchRequirementString(c.req)
		if got == nil {
			t.Errorf("parseTorchRequirementString(%q) = nil", c.req)
			continue
		}
		if got.Ceiling != c.want {
			t.Errorf("parseTorchRequirementString(%q).Ceiling = %q, want %q", c.req, got.Ceiling, c.want)
		}
	}
}

// A PEP 723 script runs in its own uv environment, so its torch bound — not
// the project lock — decides which wheel imports and therefore the cap (wb166).
func TestResolveJobTorchMaxComputeCapForPersistence_ScriptTorchBound(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		want     string
	}{
		{"upper bound", `# dependencies = ["torch>=2.2,<2.7"]`, "9.0"},
		{"wildcard pin", `# dependencies = ["torch==2.6.*"]`, "9.0"},
		{"exact pin", `# dependencies = ["torch==2.4.1"]`, "9.0"},
		{"open range falls back to project lock", `# dependencies = ["torch>=2.2"]`, "12.0"},
		{"explicit gpu-arch-max wins", "# dependencies = [\"torch>=2.2,<2.7\"]\n# [tool.weft]\n# gpu-arch-max = \"any\"", MaxComputeCapAny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.0\"]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			WriteTestTorchPin(t, dir, "2.12.1", "cu128")
			script := "# /// script\n" + c.metadata + "\n# ///\nimport torch\n"
			if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "scripts", "exp.py"), []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			got := ResolveJobTorchMaxComputeCapForPersistence(dir, "uv run --locked scripts/exp.py --phase pilot")
			if got != c.want {
				t.Fatalf("cap = %q, want %q", got, c.want)
			}
		})
	}
}
