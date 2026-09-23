package dataloc

import (
	"path/filepath"
	"strings"
)

// DirectUVRunPEP723Script returns the script path from a simple direct
// `uv run ... script.py` command when that script contains PEP 723 metadata.
// An empty result means the command's environment ownership is not proven.
func DirectUVRunPEP723Script(dir, command string) string {
	script := directUVRunScript(command)
	if script == "" || !ScriptHasPEP723Metadata(dir, command, script) {
		return ""
	}
	return script
}

// shellStepSeparators splits a command at shell list and pipeline operators;
// "||" precedes "|" so the replacer matches the longer operator.
var shellStepSeparators = strings.NewReplacer("&&", "\n", "||", "\n", "|", "\n", ";", "\n")

// UVRunPEP723Scripts returns the scripts a command runs in their own PEP 723
// script environments: every shell step that is a direct `uv run ... script.py`
// of a script carrying PEP 723 metadata, in command order. Steps such as
// `uv run python script.py` run in the project environment and are excluded.
func UVRunPEP723Scripts(dir, command string) []string {
	var out []string
	for _, step := range strings.Split(shellStepSeparators.Replace(command), "\n") {
		if script := directUVRunScript(step); script != "" && ScriptHasPEP723Metadata(dir, command, script) {
			out = append(out, script)
		}
	}
	return out
}

func directUVRunScript(command string) string {
	// Compound commands may contain another step that needs the project
	// environment. Decline inference rather than skipping its setup.
	for _, operator := range []string{"&&", "||", "|", ";", "\n"} {
		if strings.Contains(command, operator) {
			return ""
		}
	}

	tokens := strings.Fields(command)
	i := 0
	for i < len(tokens) && isShellAssignment(tokens[i]) {
		i++
	}
	if i+1 >= len(tokens) || filepath.Base(trimShellToken(tokens[i])) != "uv" || trimShellToken(tokens[i+1]) != "run" {
		return ""
	}
	i += 2

	for i < len(tokens) {
		token := trimShellToken(tokens[i])
		if token == "--" {
			i++
			break
		}
		if !strings.HasPrefix(token, "-") {
			break
		}
		if uvRunOptionTakesValue(token) && !strings.Contains(token, "=") {
			i += 2
			continue
		}
		i++
	}
	if i >= len(tokens) {
		return ""
	}
	script := trimShellToken(tokens[i])
	if !strings.HasSuffix(script, ".py") {
		return ""
	}
	return script
}

func trimShellToken(token string) string {
	return strings.Trim(token, `"'`)
}

func isShellAssignment(token string) bool {
	token = trimShellToken(token)
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func uvRunOptionTakesValue(option string) bool {
	option = strings.SplitN(option, "=", 2)[0]
	switch option {
	case "-p", "--python",
		"--with", "--with-editable", "--with-requirements",
		"--constraints", "--overrides", "--env-file",
		"--index", "--default-index", "--index-strategy",
		"--keyring-provider", "--resolution", "--prerelease",
		"--fork-strategy", "--exclude-newer", "--link-mode",
		"--compile-bytecode-timeout", "--python-platform",
		"--python-version", "--python-preference", "--config-file":
		return true
	default:
		return false
	}
}
