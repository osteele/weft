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

// shellSteps splits command into simple-command steps at unquoted list,
// pipeline, background and grouping operators (&&, ||, |, ;, &, newline,
// parentheses, backticks) and returns each step's words with quoting removed.
// Single quotes are literal; double quotes honour backslash escapes of $, `,
// ", \ and newline; an unquoted backslash escapes the next character, and
// backslash-newline continues the line. An unquoted # starting a word begins a
// comment. `&` in a redirection (2>&1, &>) is not an operator. Command
// substitution inside double quotes is not parsed. Empty steps are omitted.
func shellSteps(command string) [][]string {
	var steps [][]string
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endStep := func() {
		endWord()
		if len(words) > 0 {
			steps = append(steps, words)
			words = nil
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\':
			if i+1 < len(command) {
				i++
				if command[i] != '\n' {
					word.WriteByte(command[i])
					inWord = true
				}
			}
		case c == '\'':
			inWord = true
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				end = len(command) - i - 1
			}
			word.WriteString(command[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			for i++; i < len(command) && command[i] != '"'; i++ {
				if command[i] == '\\' && i+1 < len(command) && strings.IndexByte("$`\"\\\n", command[i+1]) >= 0 {
					i++
					if command[i] == '\n' {
						continue
					}
				}
				word.WriteByte(command[i])
			}
		case c == ' ' || c == '\t':
			endWord()
		case c == '#' && !inWord:
			for i+1 < len(command) && command[i+1] != '\n' {
				i++
			}
		case c == '&' && isRedirectionAmpersand(command, i):
			word.WriteByte(c)
			inWord = true
		case strings.IndexByte(";&|\n()`", c) >= 0:
			endStep()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endStep()
	return steps
}

// isRedirectionAmpersand reports whether the unquoted & at command[i] belongs
// to a redirection such as 2>&1, <&3 or &>log rather than being an operator.
func isRedirectionAmpersand(command string, i int) bool {
	if i > 0 && (command[i-1] == '>' || command[i-1] == '<') {
		return true
	}
	return i+1 < len(command) && command[i+1] == '>'
}

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

// ScanCommandPythonEnvs splits command into shell steps (see shellSteps) and
// classifies each. Unrecognized executables count as project-environment
// Python: treating a step as project Python can only add the project torch's
// constraints, never drop a script environment's.
func ScanCommandPythonEnvs(dir, command string) CommandPythonEnvs {
	var out CommandPythonEnvs
	for _, words := range shellSteps(command) {
		i := 0
		for i < len(words) && (isShellAssignment(words[i]) || commandWrappers[words[i]]) {
			i++
		}
		if i >= len(words) {
			continue
		}
		name := filepath.Base(words[i])
		switch {
		case name == "uv":
			if i+1 >= len(words) || words[i+1] != "run" {
				continue // uv sync, uv pip, ...: no user Python runs.
			}
			target := uvRunTarget(words[i+2:])
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
	for i, token := range tokens {
		tokens[i] = trimShellToken(token)
	}
	i := 0
	for i < len(tokens) && isShellAssignment(tokens[i]) {
		i++
	}
	if i+1 >= len(tokens) || filepath.Base(tokens[i]) != "uv" || tokens[i+1] != "run" {
		return ""
	}
	script := uvRunTarget(tokens[i+2:])
	if !strings.HasSuffix(script, ".py") {
		return ""
	}
	return script
}

// uvRunTarget returns the command or script that `uv run` executes, given the
// unquoted words after `uv run`: the first word after uv's own options.
func uvRunTarget(words []string) string {
	i := 0
	for i < len(words) {
		word := words[i]
		if word == "--" {
			i++
			break
		}
		if !strings.HasPrefix(word, "-") {
			break
		}
		if uvRunOptionTakesValue(word) && !strings.Contains(word, "=") {
			i += 2
			continue
		}
		i++
	}
	if i >= len(words) {
		return ""
	}
	return words[i]
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
