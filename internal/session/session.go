package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GenerateName generates a session name from a command
// e.g., "python train.py" -> "python-train"
// e.g., "with-gpu python script.py" -> "python-script"
func GenerateName(command string) string {
	// Skip common prefixes like 'with-gpu', 'env VAR=value'
	cleaned := command
	cleaned = regexp.MustCompile(`^with-gpu\s+`).ReplaceAllString(cleaned, "")
	cleaned = regexp.MustCompile(`^env\s+\S+\s+`).ReplaceAllString(cleaned, "")

	parts := strings.Fields(cleaned)
	if len(parts) == 0 {
		return "job"
	}

	// Get program name (first word), remove path
	prog := filepath.Base(parts[0])

	// Get first arg (second word) if it exists and isn't a flag
	var arg string
	if len(parts) > 1 && !strings.HasPrefix(parts[1], "-") {
		arg = filepath.Base(parts[1])
		// Remove extension
		if idx := strings.LastIndex(arg, "."); idx > 0 {
			arg = arg[:idx]
		}
	}

	var name string
	if arg != "" {
		name = prog + "-" + arg
	} else {
		name = prog
	}

	// Clean to alphanumeric and dash only, max 30 chars
	name = regexp.MustCompile(`[^a-zA-Z0-9-]`).ReplaceAllString(name, "")
	if len(name) > 30 {
		name = name[:30]
	}

	if name == "" {
		return "job"
	}

	return name
}

// DefaultWorkingDir returns the current working directory converted to a remote-friendly path
// /Users/osteele/code/LM2 -> ~/code/LM2
func DefaultWorkingDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(cwd, home) {
		return "~" + cwd[len(home):], nil
	}

	return cwd, nil
}

// LogFile returns the log file path for a session
func LogFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.log", sessionName)
}

// StatusFile returns the status file path for a session
func StatusFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.status", sessionName)
}

// MetadataFile returns the metadata file path for a session
func MetadataFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.meta", sessionName)
}

// ParseMetadata parses a metadata file content into key-value pairs
func ParseMetadata(content string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			result[key] = value
		}
	}
	return result
}

// FormatMetadata formats metadata as key=value pairs
func FormatMetadata(workingDir, command, host, description string, startTime int64) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("working_dir=%s", workingDir))
	lines = append(lines, fmt.Sprintf("command=%s", command))
	lines = append(lines, fmt.Sprintf("start_time=%d", startTime))
	lines = append(lines, fmt.Sprintf("host=%s", host))
	if description != "" {
		lines = append(lines, fmt.Sprintf("description=%s", description))
	}
	return strings.Join(lines, "\n")
}
