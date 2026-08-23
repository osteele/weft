package slack

import (
	"os"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/scripts"
)

func TestBuildRunnerEnvPrefixExcludesWebhook(t *testing.T) {
	t.Setenv("WEFT_SLACK_WEBHOOK", "https://hooks.example/should-not-appear")
	t.Setenv("WEFT_SLACK_VERBOSE", "1")
	t.Setenv("WEFT_SLACK_NOTIFY", "failures")
	t.Setenv("WEFT_SLACK_MIN_DURATION", "30")

	got := BuildRunnerEnvPrefix()
	if strings.Contains(got, "hooks.example") || strings.Contains(got, "WEFT_SLACK_WEBHOOK") {
		t.Fatalf("runner environment contains webhook: %q", got)
	}
	for _, want := range []string{
		"WEFT_SLACK_VERBOSE=1 ",
		"WEFT_SLACK_NOTIFY='failures' ",
		"WEFT_SLACK_MIN_DURATION='30' ",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runner environment %q does not contain %q", got, want)
		}
	}
}

func TestDeployNotifyScriptWritesRestrictedCredentialFile(t *testing.T) {
	originalRunSSH := runSSHFunc
	originalCopyTo := copyToFunc
	t.Cleanup(func() {
		runSSHFunc = originalRunSSH
		copyToFunc = originalCopyTo
	})

	const webhook = "https://hooks.example/test"
	var commands []string
	var configContent string
	var configMode os.FileMode
	runSSHFunc = func(host, command string) (string, string, error) {
		if host != "studio" {
			t.Fatalf("host = %q, want studio", host)
		}
		commands = append(commands, command)
		return "", "", nil
	}
	copyToFunc = func(localPath, host, remotePath string) error {
		if host != "studio" {
			t.Fatalf("host = %q, want studio", host)
		}
		if remotePath != notifyConfigTempPath {
			return nil
		}
		data, err := os.ReadFile(localPath)
		if err != nil {
			t.Fatalf("read staged config: %v", err)
		}
		info, err := os.Stat(localPath)
		if err != nil {
			t.Fatalf("stat staged config: %v", err)
		}
		configContent = string(data)
		configMode = info.Mode().Perm()
		return nil
	}

	DeployNotifyScript("studio", webhook)

	if configContent != "WEFT_SLACK_WEBHOOK="+webhook+"\n" {
		t.Fatalf("config content = %q", configContent)
	}
	if configMode != 0o600 {
		t.Fatalf("staged config mode = %o, want 600", configMode)
	}
	commandText := strings.Join(commands, "\n")
	if strings.Contains(commandText, webhook) {
		t.Fatal("webhook appears in an SSH command")
	}
	for _, want := range []string{
		"chmod 700 ~/.config/weft",
		"umask 077; : > " + notifyConfigTempPath,
		"chmod 600 " + notifyConfigTempPath + " && mv -f " + notifyConfigTempPath + " " + NotifyConfigPath,
	} {
		if !strings.Contains(commandText, want) {
			t.Fatalf("SSH commands do not contain %q:\n%s", want, commandText)
		}
	}
}

func TestDeployNotifyScriptRemovesCredentialWhenDisabled(t *testing.T) {
	originalRunSSH := runSSHFunc
	originalCopyTo := copyToFunc
	t.Cleanup(func() {
		runSSHFunc = originalRunSSH
		copyToFunc = originalCopyTo
	})

	var commands []string
	var copiedConfig bool
	runSSHFunc = func(host, command string) (string, string, error) {
		commands = append(commands, command)
		return "", "", nil
	}
	copyToFunc = func(localPath, host, remotePath string) error {
		if remotePath == notifyConfigTempPath {
			copiedConfig = true
		}
		return nil
	}

	DeployNotifyScript("studio", "")

	if copiedConfig {
		t.Fatal("disabled notifications copied a credential file")
	}
	want := "rm -f " + NotifyConfigPath + " " + notifyConfigTempPath
	if !strings.Contains(strings.Join(commands, "\n"), want) {
		t.Fatalf("SSH commands do not contain %q", want)
	}
}

func TestNotifyScriptReadsManagedFileOnly(t *testing.T) {
	script := string(scripts.NotifySlackScript)
	if !strings.Contains(script, "$HOME/.config/weft/notify-slack.env") {
		t.Fatal("notify script does not read the managed credential file")
	}
	for _, unwanted := range []string{"${WEFT_SLACK_WEBHOOK", "~/.config/weft/config"} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("notify script still reads %q", unwanted)
		}
	}
}
