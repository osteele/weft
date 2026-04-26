// Package aiassist invokes Claude Code (`claude -p`) to produce job
// summaries, follow-up suggestions, and remediation choices for the list TUI.
//
// First-pass call: Assist returns a JSON-decoded AssistResult.
// Second-pass call: ExecuteChoices runs the user-selected prompts and streams
// stdout back via a callback.
package aiassist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/util"
)

// Kind selects which prompt to send and how to interpret the response.
type Kind int

const (
	KindProgress Kind = iota // running job
	KindSuccess              // completed, exit 0
	KindFailure              // completed, exit != 0
)

// Label returns a short user-facing label for the assist kind, suitable for
// the overlay title bar.
func (k Kind) Label() string {
	switch k {
	case KindProgress:
		return "progress"
	case KindSuccess:
		return "review"
	case KindFailure:
		return "remediate"
	}
	return "unknown"
}

type Choice struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

type AssistResult struct {
	Summary string   `json:"summary"`
	ETA     string   `json:"eta,omitempty"`
	Choices []Choice `json:"choices,omitempty"`
}

// KindForJob returns the appropriate Kind for the job's current status, or
// false if the job is in a state where assist is not meaningful.
func KindForJob(job *db.Job) (Kind, bool) {
	if job == nil {
		return 0, false
	}
	switch job.EffectiveStatus() {
	case db.StatusRunning:
		return KindProgress, true
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return KindFailure, true
		}
		return KindSuccess, true
	case db.StatusFailed, db.StatusDead:
		return KindFailure, true
	}
	return 0, false
}

// Assist runs the first-pass claude invocation.
func Assist(ctx context.Context, job *db.Job, kind Kind) (AssistResult, error) {
	if job == nil {
		return AssistResult{}, errors.New("aiassist: nil job")
	}
	cwd := resolveCWD(job)
	stdin := captureJobContext(ctx, job.ID)
	out, err := runClaude(ctx, cwd, promptFor(kind, job.ID), stdin, nil)
	if err != nil {
		return AssistResult{}, err
	}
	var res AssistResult
	if kind == KindProgress {
		res.Summary = strings.TrimSpace(string(out))
		return res, nil
	}
	if perr := unmarshalLoose(out, &res); perr != nil {
		return AssistResult{}, fmt.Errorf("parse claude JSON: %w (output: %q)", perr, truncate(string(out), 400))
	}
	return res, nil
}

// ExecuteChoices runs the second-pass invocation.
func ExecuteChoices(ctx context.Context, job *db.Job, choices []Choice, onChunk func(string)) (string, error) {
	if job == nil {
		return "", errors.New("aiassist: nil job")
	}
	if len(choices) == 0 {
		return "", errors.New("aiassist: no choices selected")
	}
	cwd := resolveCWD(job)
	stdin := captureJobContext(ctx, job.ID)
	prompt := buildChoicePrompt(choices, job.ID)
	out, err := runClaudeStream(ctx, cwd, prompt, stdin, onChunk)
	return string(out), err
}

func buildChoicePrompt(choices []Choice, jobID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Carry out the following actions for weft job wj%d. Report briefly on what you did.\n\n", jobID)
	for i, c := range choices {
		fmt.Fprintf(&b, "Action %d — %s\n%s\n\n", i+1, c.Title, c.Prompt)
	}
	return b.String()
}

// captureJobContext runs `weft info` and `weft status` for the job and
// returns their combined stdout, to feed claude on stdin as a head-start so
// it does not need to spend a turn calling the same commands itself. Errors
// are silently swallowed — claude can fall back to invoking weft itself.
//
// WEFT_BIN overrides the binary path; setting it to empty disables capture
// (useful for tests, since os.Executable() returns the test binary, which
// would recursively spawn itself).
func captureJobContext(ctx context.Context, jobID int64) []byte {
	bin, ok := weftBin()
	if !ok {
		return nil
	}
	id := fmt.Sprintf("wj%d", jobID)
	var b bytes.Buffer
	for _, args := range [][]string{{"info", id}, {"status", id}} {
		fmt.Fprintf(&b, "$ weft %s\n", strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, bin, args...)
		out, _ := cmd.CombinedOutput()
		b.Write(out)
		b.WriteString("\n")
	}
	return b.Bytes()
}

func weftBin() (string, bool) {
	if v, set := os.LookupEnv("WEFT_BIN"); set {
		if v == "" {
			return "", false
		}
		return v, true
	}
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if filepath.Base(self) != "weft" {
		return "", false
	}
	return self, true
}

func resolveCWD(job *db.Job) string {
	dir := strings.TrimSpace(job.EffectiveWorkingDir())
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// claudeBin returns the claude binary to invoke. WEFT_CLAUDE_BIN overrides
// the default for tests and wrappers.
func claudeBin() string {
	if v := strings.TrimSpace(os.Getenv("WEFT_CLAUDE_BIN")); v != "" {
		return v
	}
	return "claude"
}

// runClaude invokes claude -p with --permission-mode auto in plain text mode
// and returns the buffered stdout. Used for the first-pass JSON response.
func runClaude(ctx context.Context, cwd, prompt string, stdin []byte, onChunk func(string)) ([]byte, error) {
	cmd, stderrBuf, err := newClaudeCmd(ctx, cwd, prompt, stdin)
	if err != nil {
		return nil, err
	}
	if onChunk == nil {
		out, err := cmd.Output()
		if err != nil {
			return nil, claudeErr(err, stderrBuf)
		}
		return out, nil
	}
	return runStreaming(cmd, stderrBuf, func(line string) { onChunk(line) })
}

// runClaudeStream invokes claude with --output-format stream-json so the
// caller can show progress while the agent edits files and runs tools.
// Stream events are parsed and emitted via onChunk as short, human-readable
// lines (e.g. "● Edit foo.py", "→ test passed", assistant text).
func runClaudeStream(ctx context.Context, cwd, prompt string, stdin []byte, onChunk func(string)) ([]byte, error) {
	cmd, stderrBuf, err := newClaudeCmd(ctx, cwd, prompt, stdin, "--output-format", "stream-json", "--verbose")
	if err != nil {
		return nil, err
	}
	emit := onChunk
	if emit == nil {
		emit = func(string) {}
	}
	return runStreaming(cmd, stderrBuf, func(line string) {
		for _, chunk := range parseStreamEvent(line) {
			emit(chunk)
		}
	})
}

func newClaudeCmd(ctx context.Context, cwd, prompt string, stdin []byte, extraArgs ...string) (*exec.Cmd, *bytes.Buffer, error) {
	bin := claudeBin()
	if _, err := exec.LookPath(bin); err != nil {
		return nil, nil, fmt.Errorf("%s not found on PATH; install Claude Code or set WEFT_CLAUDE_BIN", bin)
	}
	args := append([]string{"-p", prompt, "--permission-mode", "auto"}, extraArgs...)
	cmd := exec.CommandContext(ctx, bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stdin = bytes.NewReader(stdin)
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = stderrBuf
	return cmd, stderrBuf, nil
}

func runStreaming(cmd *exec.Cmd, stderrBuf *bytes.Buffer, onLine func(string)) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	var collected bytes.Buffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		collected.WriteString(line)
		collected.WriteByte('\n')
		onLine(line)
	}
	if err := cmd.Wait(); err != nil {
		return collected.Bytes(), claudeErr(err, stderrBuf)
	}
	return collected.Bytes(), nil
}

// streamEvent matches the subset of claude --output-format stream-json that
// we render. Unknown events are ignored.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Message *struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

// parseStreamEvent renders one stream-json event into 0+ user-visible lines.
func parseStreamEvent(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []string{line} // fall back to raw
	}
	switch ev.Type {
	case "system":
		return nil
	case "assistant":
		if ev.Message == nil {
			return nil
		}
		var out []string
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				txt := strings.TrimSpace(c.Text)
				if txt != "" {
					out = append(out, txt)
				}
			case "tool_use":
				out = append(out, "● "+formatToolUse(c.Name, c.Input))
			}
		}
		return out
	case "user":
		// tool results — keep brief
		return nil
	case "result":
		if ev.Subtype != "" && ev.Subtype != "success" {
			return []string{"⚠ " + ev.Subtype}
		}
		return nil
	}
	return nil
}

func formatToolUse(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return name
	}
	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err != nil {
		return name
	}
	switch name {
	case "Bash":
		if cmd, ok := fields["command"].(string); ok {
			return "Bash: " + truncate(cmd, 80)
		}
	case "Edit", "Write", "Read":
		if p, ok := fields["file_path"].(string); ok {
			return name + " " + p
		}
	case "Glob", "Grep":
		if p, ok := fields["pattern"].(string); ok {
			return name + " " + truncate(p, 60)
		}
	}
	return name
}

func claudeErr(err error, stderr *bytes.Buffer) error {
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		return fmt.Errorf("claude: %w", err)
	}
	return fmt.Errorf("claude: %w: %s", err, truncate(msg, 400))
}

// unmarshalLoose handles claude responses that occasionally wrap JSON in
// stray prose or markdown fences despite the prompt's instructions.
func unmarshalLoose(data []byte, v any) error {
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	if i := strings.Index(s, "{"); i > 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	return json.Unmarshal([]byte(s), v)
}

func truncate(s string, n int) string {
	return util.Truncate(s, n)
}
