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

// shellStepSeparators splits a command at shell list and pipeline operators,
// after joining backslash line continuations; "||" precedes "|" so the
// replacer matches the longer operator.
var shellStepSeparators = strings.NewReplacer("\\\n", " ", "&&", "\n", "||", "\n", "|", "\n", ";", "\n")

// CommandPythonEnvs classifies the Python environments a shell command's
// steps run in.
type CommandPythonEnvs struct {
	// Scripts are the PEP 723 scripts run in their own script environments
	// by direct `uv run ... script.py` steps, in command order.
	Scripts []string
	// ProjectEnv reports that some step may run Python in the project
	// environment: `uv run python ...`, `uv run` of a tool or of a script
	// without PEP 723 metadata, bare `python ...`, or any executable that is
	// not a known non-Python command.
	ProjectEnv bool
}

// neutralCommands are shell builtins and utilities that never import the
// project's Python packages, so a step running one says nothing about which
// environment the job's torch comes from.
var neutralCommands = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "pwd": true, "export": true,
	"unset": true, "set": true, "source": true, ".": true, "true": true,
	"false": true, "test": true, "[": true, "echo": true, "printf": true,
	"mkdir": true, "rm": true, "cp": true, "mv": true, "ln": true,
	"touch": true, "ls": true, "cat": true, "tee": true, "sleep": true,
	"date": true, "nvidia-smi": true, "uvx": true,
}

// commandWrappers run the rest of the step as the command.
var commandWrappers = map[string]bool{"env": true, "time": true, "nohup": true, "exec": true}

// ScanCommandPythonEnvs splits command into shell steps and classifies each.
// Unrecognized executables count as project-environment Python: treating a
// step as project Python can only add the project torch's constraints, never
// drop a script environment's.
func ScanCommandPythonEnvs(dir, command string) CommandPythonEnvs {
	var out CommandPythonEnvs
	for _, step := range strings.Split(shellStepSeparators.Replace(command), "\n") {
		tokens := strings.Fields(step)
		i := 0
		for i < len(tokens) && (isShellAssignment(tokens[i]) || commandWrappers[trimStepToken(tokens[i])]) {
			i++
		}
		if i >= len(tokens) {
			continue
		}
		name := filepath.Base(trimStepToken(tokens[i]))
		switch {
		case name == "uv":
			if i+1 >= len(tokens) || trimShellToken(tokens[i+1]) != "run" {
				continue // uv sync, uv pip, ...: no user Python runs.
			}
			target := trimStepToken(uvRunTarget(tokens[i+2:]))
			if strings.HasSuffix(target, ".py") && ScriptHasPEP723Metadata(dir, command, target) {
				out.Scripts = append(out.Scripts, target)
			} else {
				out.ProjectEnv = true
			}
		case neutralCommands[name]:
		default:
			out.ProjectEnv = true
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
	script := uvRunTarget(tokens[i+2:])
	if !strings.HasSuffix(script, ".py") {
		return ""
	}
	return script
}

// uvRunTarget returns the command or script that `uv run` executes, given the
// tokens after `uv run`: the first token after uv's own options.
func uvRunTarget(tokens []string) string {
	i := 0
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
	return trimShellToken(tokens[i])
}

// trimStepToken strips quotes and subshell parentheses from a step token.
func trimStepToken(token string) string {
	return strings.Trim(token, `"'()`)
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
