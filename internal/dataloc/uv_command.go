package dataloc

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
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

// scanShellPythonSteps parses command as bash and classifies every simple
// command, including those in command substitutions, subshells, pipelines,
// lists and compound statements; heredoc bodies are data. It returns the PEP
// 723 scripts run in their own environments by direct `uv run [opts] X.py`
// steps, and whether some step may run Python in the project environment:
// `uv run python ...`, `uv run` of a tool or of a script without PEP 723
// metadata, bare `python ...`, or any executable that is not a known
// non-Python command.
//
// A command that fails to parse, or a command or `uv run` target that is not a
// static word, counts as project Python: that can only add the project torch's
// constraints, never drop a script environment's.
func scanShellPythonSteps(dir, command string) (scripts []string, projectPython bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, true
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		script, project := classifyShellCall(dir, command, call)
		if script != "" {
			scripts = append(scripts, script)
		}
		projectPython = projectPython || project
		return true
	})
	return scripts, projectPython
}

// classifyShellCall returns the PEP 723 script a simple command runs in its
// own environment, or whether it may run project-environment Python.
func classifyShellCall(dir, command string, call *syntax.CallExpr) (script string, projectPython bool) {
	words := make([]string, 0, len(call.Args))
	for _, arg := range call.Args {
		word, ok := literalWord(arg)
		if !ok {
			if len(words) == 0 {
				return "", true // The executable is not statically known.
			}
			word = "\x00" // A dynamic argument never names a known option or script.
		}
		words = append(words, word)
	}
	i := 0
	for i < len(words) && (isShellAssignment(words[i]) || commandWrappers[words[i]]) {
		i++
	}
	if i >= len(words) {
		return "", false // Assignments only.
	}
	switch name := filepath.Base(words[i]); {
	case name == "uv":
		if i+1 >= len(words) || words[i+1] != "run" {
			return "", false // uv sync, uv pip, ...: no user Python runs.
		}
		target := uvRunTarget(words[i+2:])
		if strings.HasSuffix(target, ".py") && ScriptHasPEP723Metadata(dir, command, target) {
			return target, false
		}
		return "", true
	case neutralCommands[name]:
		return "", false
	default:
		return "", true
	}
}

// literalWord returns w's value with quoting removed when w is a static
// string, and ok=false when it contains an expansion or substitution.
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeShell(p.Value, ""))
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false // $'...' applies ANSI-C escapes.
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, qp := range p.Parts {
				lit, ok := qp.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(unescapeShell(lit.Value, "$`\"\\\n"))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescapeShell removes backslash escapes from literal shell text. Outside
// double quotes (escapable == "") a backslash escapes any character; inside
// them it escapes only the characters in escapable. An escaped newline is a
// line continuation and is dropped.
func unescapeShell(s, escapable string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (escapable == "" || strings.IndexByte(escapable, s[i+1]) >= 0) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
