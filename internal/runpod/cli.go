package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type cliRunner struct {
	cliPath     string
	lookPath    func(string) (string, error)
	runOutput   func(context.Context, string, ...string) ([]byte, error)
	runCombined func(context.Context, string, ...string) ([]byte, error)
}

type cliCapabilities struct {
	path                  string
	version               string
	searchCommand         []string
	podListCommand        []string
	podGetCommand         []string
	podDeleteCommand      []string
	templateListCommand   []string
	templateGetCommand    []string
	templateCreateCommand []string
}

func newCLIRunner(cliPath string) *cliRunner {
	return &cliRunner{
		cliPath:  cliPath,
		lookPath: exec.LookPath,
		runOutput: func(ctx context.Context, path string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			out, err := cmd.Output()
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					prefix := args
					if len(prefix) > 3 {
						prefix = args[:3]
					}
					return nil, fmt.Errorf("%s: %s", strings.Join(prefix, " "), strings.TrimSpace(string(exitErr.Stderr)))
				}
				return nil, err
			}
			return out, nil
		},
		runCombined: func(ctx context.Context, path string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				prefix := args
				if len(prefix) > 3 {
					prefix = args[:3]
				}
				return out, fmt.Errorf("%s: %s", strings.Join(prefix, " "), strings.TrimSpace(string(out)))
			}
			return out, nil
		},
	}
}

func (r *cliRunner) detectCapabilities(ctx context.Context) (*cliCapabilities, error) {
	path, err := r.lookPath(r.cliPath)
	if err != nil {
		return nil, fmt.Errorf("runpodctl not found in PATH (install: https://docs.runpod.io/cli/install)")
	}

	versionOut, err := r.runCombined(ctx, path, "version")
	if err != nil {
		return nil, fmt.Errorf("runpodctl version check failed: %w", err)
	}

	caps := &cliCapabilities{
		path:    path,
		version: strings.TrimSpace(string(versionOut)),
	}

	getHelp, _ := r.runCombined(ctx, path, "get", "--help")
	gpuHelp, _ := r.runCombined(ctx, path, "gpu", "--help")
	podHelp, _ := r.runCombined(ctx, path, "pod", "--help")
	templateHelp, _ := r.runCombined(ctx, path, "template", "--help")

	switch {
	case containsCommand(gpuHelp, "list"):
		caps.searchCommand = []string{"gpu", "list"}
	case containsCommand(getHelp, "cloud"):
		caps.searchCommand = []string{"get", "cloud"}
	case containsCommand(getHelp, "gpu"):
		caps.searchCommand = []string{"get", "gpu"}
	}

	switch {
	case containsCommand(podHelp, "list") && containsCommand(podHelp, "get") && containsCommand(podHelp, "delete"):
		caps.podListCommand = []string{"pod", "list", "--all"}
		caps.podGetCommand = []string{"pod", "get"}
		caps.podDeleteCommand = []string{"pod", "delete"}
	case containsCommand(getHelp, "pod"):
		caps.podListCommand = []string{"get", "pod"}
		caps.podGetCommand = []string{"get", "pod"}
		caps.podDeleteCommand = []string{"remove", "pod"}
	}

	if containsCommand(templateHelp, "list") && containsCommand(templateHelp, "get") && containsCommand(templateHelp, "create") {
		caps.templateListCommand = []string{"template", "list", "--type", "user"}
		caps.templateGetCommand = []string{"template", "get"}
		caps.templateCreateCommand = []string{"template", "create"}
	}

	if len(caps.searchCommand) == 0 || len(caps.podListCommand) == 0 || len(caps.podGetCommand) == 0 || len(caps.podDeleteCommand) == 0 {
		return nil, fmt.Errorf("runpodctl is installed but does not expose the required cloud/pod commands")
	}
	return caps, nil
}

func containsCommand(help []byte, command string) bool {
	for _, line := range strings.Split(strings.ToLower(string(help)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == strings.ToLower(command) {
			return true
		}
	}
	return false
}

func decodeJSONArray(data []byte) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if payload := extractJSONPayload(trimmed, '[', ']'); payload != "" {
		trimmed = payload
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func decodeJSONObject(data []byte) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if payload := extractJSONPayload(trimmed, '{', '}'); payload != "" {
		trimmed = payload
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func extractJSONPayload(s string, start, end byte) string {
	if s == "" {
		return ""
	}
	if s[0] == start {
		return s
	}
	i := strings.IndexByte(s, start)
	j := strings.LastIndexByte(s, end)
	if i == -1 || j == -1 || j < i {
		return ""
	}
	return strings.TrimSpace(s[i : j+1])
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := data[key]; ok {
			switch vv := v.(type) {
			case string:
				if strings.TrimSpace(vv) != "" {
					return vv
				}
			case float64:
				if vv == float64(int64(vv)) {
					return strconv.FormatInt(int64(vv), 10)
				}
			case []any:
				parts := make([]string, 0, len(vv))
				for _, item := range vv {
					if s, ok := item.(string); ok {
						parts = append(parts, s)
					}
				}
				if joined := strings.Join(parts, " "); strings.TrimSpace(joined) != "" {
					return joined
				}
			}
		}
	}
	return ""
}

func firstFloat(data map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if v, ok := data[key]; ok {
			switch vv := v.(type) {
			case float64:
				return vv
			case int64:
				return float64(vv)
			case int:
				return float64(vv)
			case json.Number:
				f, _ := vv.Float64()
				return f
			case string:
				f, err := strconv.ParseFloat(vv, 64)
				if err == nil {
					return f
				}
			}
		}
	}
	return 0
}

func firstInt(data map[string]any, keys ...string) int {
	for _, key := range keys {
		if v, ok := data[key]; ok {
			switch vv := v.(type) {
			case float64:
				return int(vv)
			case int:
				return vv
			case int64:
				return int(vv)
			case json.Number:
				i, _ := vv.Int64()
				return int(i)
			case string:
				i, err := strconv.Atoi(vv)
				if err == nil {
					return i
				}
			}
		}
	}
	return 0
}

func firstBool(data map[string]any, keys ...string) bool {
	for _, key := range keys {
		if v, ok := data[key]; ok {
			switch vv := v.(type) {
			case bool:
				return vv
			case string:
				return strings.EqualFold(vv, "true")
			}
		}
	}
	return false
}

func joinCommand(args []string) string {
	return strings.Join(args, " ")
}

func runWithTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(timeoutCtx)
}

func isOfferUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTemplateNotFound) {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no longer exists"):
		return true
	case strings.Contains(msg, "offer unavailable"):
		return true
	case strings.Contains(msg, "offer") && strings.Contains(msg, "unavailable"):
		return true
	case strings.Contains(msg, "gpu") && strings.Contains(msg, "unavailable"):
		return true
	case strings.Contains(msg, "no longer any instances available"):
		return true
	case strings.Contains(msg, "no instances available"):
		return true
	default:
		return false
	}
}
