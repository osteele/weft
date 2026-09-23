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
		// Wildcard exclusions remove whole release lines below the bound.
		{"torch>=2.2,<2.8,!=2.7.*", "2.6"},
		{"torch<2.8,!=2.7.*,!=2.6.*", "2.5"},
		// Excluding one release leaves the rest of its line admitted.
		{"torch>=2.2,<2.8,!=2.7.0", "2.7"},
		// No bound on the release line: the range can resolve the newest torch.
		{"torch>=2.2", ""},
		{"torch~=2.6", ""},
		{"torch>=2.2,<3", ""},
		{"torch==2.*", ""},
		{"torch!=2.5.0", ""},
		// No admitted line can be established: never report an excluded one.
		{"torch>=2.7,<2.8,!=2.7.*", ""},
		{"torch<2.1,!=2.0.*", ""},
		{"torch<2.8,!=2.*", ""},
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

// A direct `uv run script.py` imports torch from the script's own PEP 723
// environment, so the script's torch bound — not the project lock — decides
// which wheel imports and therefore the cap (wb166). `uv run python script.py`
// and other project-environment steps import the project lock's torch, so a
// command mixing both kinds of step must satisfy both.
func TestResolveJobTorchMaxComputeCapForPersistence_ScriptTorchBound(t *testing.T) {
	const bounded = `# dependencies = ["torch>=2.2,<2.7"]`
	const torch291 = `# dependencies = ["torch==2.9.1"]`
	cases := []struct {
		name         string
		projectTorch string
		scripts      map[string]string
		command      string
		want         string
	}{
		{"upper bound", "2.12.1", map[string]string{"exp.py": bounded}, "uv run --locked scripts/exp.py --phase pilot", "9.0"},
		{"wildcard pin", "2.12.1", map[string]string{"exp.py": `# dependencies = ["torch==2.6.*"]`}, "uv run scripts/exp.py", "9.0"},
		{"exact pin", "2.12.1", map[string]string{"exp.py": `# dependencies = ["torch==2.4.1"]`}, "uv run scripts/exp.py", "9.0"},
		{"wildcard exclusion below the bound", "2.12.1", map[string]string{"exp.py": `# dependencies = ["torch>=2.2,<2.8,!=2.7.*"]`}, "uv run scripts/exp.py", "9.0"},
		{"open range falls back to project lock", "2.12.1", map[string]string{"exp.py": `# dependencies = ["torch>=2.2"]`}, "uv run scripts/exp.py", "12.0"},
		{"explicit gpu-arch-max wins", "2.12.1", map[string]string{"exp.py": bounded + "\n# [tool.weft]\n# gpu-arch-max = \"any\""}, "uv run scripts/exp.py", MaxComputeCapAny},
		{"uv run python uses the project env", "2.6.0", map[string]string{"exp.py": `# dependencies = ["torch==2.8.0"]`}, "uv run python scripts/exp.py", "9.0"},
		{
			"compound command takes the most restrictive script env", "2.12.1",
			map[string]string{"first.py": `# dependencies = ["torch==2.9.1"]`, "second.py": bounded},
			"uv run scripts/first.py && uv run scripts/second.py", "9.0",
		},
		{
			"project-env step keeps the project cap in the minimum", "2.6.0",
			map[string]string{"first.py": `# dependencies = ["torch==2.6.0"]`, "second.py": torch291},
			"uv run python scripts/first.py && uv run scripts/second.py", "9.0",
		},
		{
			"bare python step runs in the project env", "2.6.0",
			map[string]string{"prep.py": "", "second.py": torch291},
			"python scripts/prep.py && uv run scripts/second.py", "9.0",
		},
		{
			"uv run tool step runs in the project env", "2.6.0",
			map[string]string{"second.py": torch291},
			"uv run pytest -q && uv run scripts/second.py", "9.0",
		},
		{
			"cd keeps the script-only override", "2.6.0",
			map[string]string{"second.py": torch291},
			"cd scripts && uv run second.py", "12.0",
		},
		{
			"non-Python steps are neutral", "2.6.0",
			map[string]string{"second.py": torch291},
			"echo start && mkdir -p out && FOO=1 uv run scripts/second.py | tee out/log", "12.0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.0\"]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			WriteTestTorchPin(t, dir, c.projectTorch, "cu128")
			if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, metadata := range c.scripts {
				script := "import torch\n"
				if metadata != "" {
					script = "# /// script\n" + metadata + "\n# ///\n" + script
				}
				if err := os.WriteFile(filepath.Join(dir, "scripts", name), []byte(script), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := ResolveJobTorchMaxComputeCapForPersistence(dir, c.command); got != c.want {
				t.Fatalf("cap = %q, want %q", got, c.want)
			}
		})
	}
}
