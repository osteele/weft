package coordinator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

const launchdLabel = "com.osteele.weft.coordinator"

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Binary}}</string>
		<string>coordinator</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/coordinator.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/coordinator.stderr.log</string>
</dict>
</plist>
`))

type plistData struct {
	Label  string
	Binary string
	LogDir string
}

// PlistPath returns the path to the launchd plist file.
func PlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// Install creates the launchd plist and loads it.
func Install() error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	// Resolve symlinks to get the actual binary path
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".cache", "weft")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	plistPath := PlistPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}

	f, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	defer f.Close()

	data := plistData{
		Label:  launchdLabel,
		Binary: binary,
		LogDir: logDir,
	}
	if err := plistTemplate.Execute(f, data); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	// Load the plist
	cmd := exec.Command("launchctl", "load", plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

// Uninstall stops and removes the launchd plist.
func Uninstall() error {
	plistPath := PlistPath()

	// Unload first (ignore error if not loaded)
	cmd := exec.Command("launchctl", "unload", plistPath)
	cmd.CombinedOutput() // ignore error

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	return nil
}

// IsInstalled checks whether the launchd plist exists.
func IsInstalled() bool {
	_, err := os.Stat(PlistPath())
	return err == nil
}
