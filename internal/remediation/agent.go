package remediation

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

// AgentResult holds the output from a coding agent invocation.
type AgentResult struct {
	Patch    string // git/jj diff output after agent ran
	Response string // agent's stdout
	Success  bool   // whether the agent exited successfully
}

// InvokeCodingAgent runs the configured coding agent to fix a code error.
// Only called when cfg.CodingAgent is non-empty (opt-in).
func InvokeCodingAgent(cfg config.RemediationConfig, job *db.Job, diagnosis *ErrorDiagnosis) (*AgentResult, error) {
	if cfg.CodingAgent == "" {
		return nil, fmt.Errorf("no coding agent configured")
	}

	prompt := buildPrompt(job, diagnosis)

	workDir := cfg.CodingAgentDir
	if workDir == "" {
		workDir = job.WorkingDir
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Split the agent command and append the prompt as stdin
	parts := strings.Fields(cfg.CodingAgent)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty coding agent command")
	}

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := &AgentResult{
		Response: stdout.String(),
		Success:  err == nil,
	}

	if err != nil {
		return result, fmt.Errorf("agent exited with error: %w (stderr: %s)", err, stderr.String())
	}

	// Capture diff of changes made by the agent
	patch, diffErr := captureWorkingDirDiff(workDir)
	if diffErr == nil {
		result.Patch = patch
	}

	return result, nil
}

// buildPrompt constructs the prompt sent to the coding agent.
func buildPrompt(job *db.Job, diagnosis *ErrorDiagnosis) string {
	// Take last 50 lines of the details (the matched error text)
	errorLines := diagnosis.Details
	lines := strings.Split(errorLines, "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
		errorLines = strings.Join(lines, "\n")
	}

	return fmt.Sprintf(`The following job failed on host %s:
  Command: %s
  Working directory: %s

Error output:
%s

Error diagnosis: %s

Please fix the code to resolve this error. Make minimal changes only.
`, job.Host, job.Command, job.WorkingDir, errorLines, diagnosis.Message)
}

// captureWorkingDirDiff runs git diff or jj diff in the working directory.
func captureWorkingDirDiff(dir string) (string, error) {
	// Try jj first
	cmd := exec.Command("jj", "diff")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		return string(out), nil
	}

	// Fall back to git
	cmd = exec.Command("git", "diff")
	cmd.Dir = dir
	out, err = cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
