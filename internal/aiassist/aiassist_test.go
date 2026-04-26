package aiassist

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

// stubClaude writes a tiny shell script that prints a fixed payload, ignoring
// stdin and args. Returns the path. Skips on Windows.
func stubClaude(t *testing.T, payload string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub not supported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-stub.sh")
	script := "#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n" + payload + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestAssist_ProgressReturnsTextSummary(t *testing.T) {
	stub := stubClaude(t, "Training run, step 4200/10000.\nLoss falling steadily.\nETA ~ 25 min.")
	t.Setenv("WEFT_CLAUDE_BIN", stub)
	t.Setenv("WEFT_BIN", "")

	job := &db.Job{ID: 1, Command: "uv run train.py", Status: db.StatusRunning, WorkingDir: t.TempDir()}
	res, err := Assist(context.Background(), job, KindProgress)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if !strings.Contains(res.Summary, "step 4200/10000") {
		t.Fatalf("summary missing progress line: %q", res.Summary)
	}
	if len(res.Choices) != 0 {
		t.Fatalf("progress should have no choices, got %d", len(res.Choices))
	}
}

func TestAssist_SuccessParsesJSONChoices(t *testing.T) {
	payload := `{
        "summary": "Wrote outputs/model.pt and outputs/eval.json.",
        "choices": [
            {"id":"notebook","title":"Update lab notebook","description":"Add eval row","prompt":"Append a row to lab-notebook.md."},
            {"id":"followup","title":"Run a 7B variant","description":"Same recipe at 7B","prompt":"weft run -- uv run train.py --size 7b"}
        ]
    }`
	stub := stubClaude(t, payload)
	t.Setenv("WEFT_CLAUDE_BIN", stub)
	t.Setenv("WEFT_BIN", "")

	exit := 0
	job := &db.Job{ID: 2, Command: "uv run train.py", Status: db.StatusCompleted, ExitCode: &exit, WorkingDir: t.TempDir()}
	res, err := Assist(context.Background(), job, KindSuccess)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if !strings.Contains(res.Summary, "model.pt") {
		t.Fatalf("summary not parsed: %q", res.Summary)
	}
	if len(res.Choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(res.Choices))
	}
	if res.Choices[1].ID != "followup" {
		t.Fatalf("choice ID not parsed: %+v", res.Choices[1])
	}
}

func TestAssist_FailureToleratesFencedJSON(t *testing.T) {
	payload := "Here you go:\n```json\n{\"summary\":\"OOM at step 12\",\"choices\":[{\"id\":\"bump\",\"title\":\"Bump --gpu-mem\",\"description\":\"Try 40GB\",\"prompt\":\"weft run --gpu-mem 40 ...\"}]}\n```\n"
	stub := stubClaude(t, payload)
	t.Setenv("WEFT_CLAUDE_BIN", stub)
	t.Setenv("WEFT_BIN", "")

	exit := 1
	job := &db.Job{ID: 3, Status: db.StatusFailed, ExitCode: &exit, WorkingDir: t.TempDir()}
	res, err := Assist(context.Background(), job, KindFailure)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if res.Summary != "OOM at step 12" {
		t.Fatalf("summary: %q", res.Summary)
	}
	if len(res.Choices) != 1 || res.Choices[0].ID != "bump" {
		t.Fatalf("choices not parsed: %+v", res.Choices)
	}
}

func TestKindForJob(t *testing.T) {
	cases := []struct {
		name   string
		status string
		exit   *int
		kind   Kind
		ok     bool
	}{
		{"running", db.StatusRunning, nil, KindProgress, true},
		{"completed-success", db.StatusCompleted, intPtr(0), KindSuccess, true},
		{"completed-nonzero", db.StatusCompleted, intPtr(2), KindFailure, true},
		{"failed", db.StatusFailed, intPtr(1), KindFailure, true},
		{"queued", db.StatusQueued, nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := &db.Job{Status: c.status, ExitCode: c.exit, Host: "alpha"}
			k, ok := KindForJob(j)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && k != c.kind {
				t.Fatalf("kind = %v, want %v", k, c.kind)
			}
		})
	}
}

func intPtr(i int) *int { return &i }

func TestParseStreamEvent(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{"system-init-suppressed", `{"type":"system","subtype":"init"}`, nil},
		{
			"assistant-text",
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Editing the notebook."}]}}`,
			[]string{"Editing the notebook."},
		},
		{
			"tool-use-edit",
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/p/notes.md"}}]}}`,
			[]string{"● Edit /p/notes.md"},
		},
		{
			"tool-use-bash",
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}`,
			[]string{"● Bash: ls -la"},
		},
		{"result-success-suppressed", `{"type":"result","subtype":"success"}`, nil},
		{"result-error", `{"type":"result","subtype":"error_max_turns"}`, []string{"⚠ error_max_turns"}},
		{"non-json-fallback", `not json`, []string{"not json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStreamEvent(c.line)
			if len(got) != len(c.want) {
				t.Fatalf("len = %d (%v), want %d (%v)", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
