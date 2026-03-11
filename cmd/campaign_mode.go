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
	return resolveCampaignTUIMode(forceTUI, forcePlain, hasCampaignTerminalIO(), inCampaignAgentContext())
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
	for _, envVar := range campaignAgentEnvVars {
		if isTruthyEnv(os.Getenv(envVar)) {
			return true
		}
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
