package cmd

import (
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

var campaignAgentEnvVars = []string{
	"CLAUDECODE",
	"CODEX_CI",
	"GEMINI_CLI",
}

func resolveCampaignTUI(forceTUI, forcePlain bool) (bool, error) {
	hasTerminal := hasCampaignTerminalIO()
	return resolveCampaignTUIMode(forceTUI, forcePlain, hasTerminal, inCampaignAgentContextWithTerminal(hasTerminal))
}

func resolveCampaignTUIMode(forceTUI, forcePlain, hasTerminal, codingAgent bool) (bool, error) {
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

func hasCampaignTerminalIO() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func inCampaignAgentContext() bool {
	return inCampaignAgentContextWithTerminal(hasCampaignTerminalIO())
}

func inCampaignAgentContextWithTerminal(hasTerminal bool) bool {
	for _, envVar := range campaignAgentEnvVars {
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
