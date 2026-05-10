package daemoncontrol

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

var plistTemplate = template.Must(template.New("daemon-plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Binary}}</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.StdoutLog}}</string>
	<key>StandardErrorPath</key>
	<string>{{.StderrLog}}</string>
</dict>
</plist>
`))

type plistData struct {
	Label     string
	Binary    string
	StdoutLog string
	StderrLog string
}

func IsInstalled(paths Paths) bool {
	_, err := os.Stat(paths.PlistFile)
	return err == nil
}

func Install(paths Paths) error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	if err := os.MkdirAll(filepath.Dir(paths.PIDFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.PlistFile), 0o755); err != nil {
		return err
	}
	f, err := os.Create(paths.PlistFile)
	if err != nil {
		return err
	}
	if err := plistTemplate.Execute(f, plistData{
		Label:     Label,
		Binary:    binary,
		StdoutLog: paths.StdoutLog,
		StderrLog: paths.StderrLog,
	}); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return Load(paths)
}

func Uninstall(paths Paths) error {
	_ = Unload(paths)
	if err := os.Remove(paths.PlistFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func Load(paths Paths) error {
	out, err := exec.Command("launchctl", "load", paths.PlistFile).CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "already loaded") || strings.Contains(text, "Load failed: 5") {
		return exec.Command("launchctl", "start", Label).Run()
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("launchctl load: %s: %w", text, err)
}

func Unload(paths Paths) error {
	out, err := exec.Command("launchctl", "unload", paths.PlistFile).CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "Could not find specified service") ||
		strings.Contains(text, "Unload failed: 5") ||
		strings.Contains(text, "No such process") {
		return nil
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("launchctl unload: %s: %w", text, err)
}
