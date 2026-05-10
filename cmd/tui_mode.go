package cmd

import (
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

var tuiAgentEnvVars = []string{
	"CLAUDECODE",
	"CODEX_CI",
	"GEMINI_CLI",
}

func resolveTUI(forceTUI, forcePlain bool) (bool, error) {
	hasTerminal := hasTerminalIO()
	return resolveTUIMode(forceTUI, forcePlain, hasTerminal, inAgentContextWithTerminal(hasTerminal))
}

func resolveTUIMode(forceTUI, forcePlain, hasTerminal, codingAgent bool) (bool, error) {
	if forceTUI && forcePlain {
		return false, usageErrorf("--tui and --plain are mutually exclusive")
	}
	if forceTUI {
		if !hasTerminal {
			return false, usageErrorf("--tui requires an interactive terminal on stdin/stdout")
		}
		return true, nil
	}
	if forcePlain {
		return false, nil
	}
	return hasTerminal && !codingAgent, nil
}

func hasTerminalIO() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func inAgentContext() bool {
	return inAgentContextWithTerminal(hasTerminalIO())
}

func inAgentContextWithTerminal(hasTerminal bool) bool {
	for _, envVar := range tuiAgentEnvVars {
		if !isTruthyEnv(os.Getenv(envVar)) {
			continue
		}
		// CODEX_CI can be present in developer shells even for manual
		// invocations; keep TUI defaults when we're in a real terminal.
		if envVar == "CODEX_CI" && hasTerminal {
			continue
		}
		return true
	}
	return false
}

func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
