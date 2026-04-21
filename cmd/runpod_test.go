package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/runpod"
)

func TestRunpodCommandsRegistered(t *testing.T) {
	for _, path := range [][]string{
		{"runpod", "doctor"},
		{"runpod", "setup"},
		{"runpod", "template", "print-bootstrap"},
	} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil {
			t.Fatalf("Find(%v): %v", path, err)
		}
		if cmd == nil {
			t.Fatalf("Find(%v) returned nil command", path)
		}
	}
}

func TestRunRunpodDoctorPrintsDiagnosis(t *testing.T) {
	origDiagnose := runpodDiagnose
	runpodDiagnose = func(*config.Config) (*runpod.Diagnosis, error) {
		return &runpod.Diagnosis{
			SearchReady:           true,
			LaunchReady:           false,
			CLIPath:               "/opt/homebrew/bin/runpodctl",
			Version:               "runpodctl 2.1.6",
			SearchCommand:         "get cloud",
			PodCommandFamily:      "pod list --all / pod get / pod delete",
			TemplateCommandFamily: "template list --type user / template get / template create",
			DefaultImage:          "runpod/base:1.0.2-ubuntu2204",
			SearchChecks:          []runpod.Check{{Name: "auth", OK: true, Detail: "authenticated"}},
			LaunchChecks:          []runpod.Check{{Name: "runpod.default_image", OK: false, Detail: "configured image \"pytorch/pytorch:2.6\" is incompatible; expected runpod/*"}},
		}, nil
	}
	defer func() { runpodDiagnose = origDiagnose }()

	var buf bytes.Buffer
	runpodDoctorCmd.SetOut(&buf)
	runpodDoctorCmd.SetErr(&buf)

	err := runRunpodDoctor(runpodDoctorCmd, nil)
	if err == nil {
		t.Fatal("expected doctor to return a readiness error")
	}
	out := buf.String()
	for _, want := range []string{
		"RunPod readiness",
		"Search ready: yes",
		"Launch ready: no",
		"[FAIL] runpod.default_image: configured image \"pytorch/pytorch:2.6\" is incompatible; expected runpod/*",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRunpodSetupPrintsTemplateAndConfigPath(t *testing.T) {
	origSetup := runpodSetup
	runpodSetup = func(*config.Config) (*runpod.SetupResult, error) {
		return &runpod.SetupResult{
			Diagnosis: &runpod.Diagnosis{
				SearchReady:  true,
				LaunchReady:  true,
				DefaultImage: "runpod/base:1.0.2-ubuntu2204",
			},
			ConfigPath:    "/tmp/config.toml",
			UpdatedConfig: true,
		}, nil
	}
	defer func() { runpodSetup = origSetup }()

	var buf bytes.Buffer
	runpodSetupCmd.SetOut(&buf)
	runpodSetupCmd.SetErr(&buf)

	if err := runRunpodSetup(runpodSetupCmd, nil); err != nil {
		t.Fatalf("runRunpodSetup: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Updated config: /tmp/config.toml"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRunpodTemplatePrintBootstrap(t *testing.T) {
	origSpec := runpodDesiredTemplateSpec
	runpodDesiredTemplateSpec = func(*config.Config) runpod.BootstrapTemplateSpec {
		return runpod.BootstrapTemplateSpec{
			Image:        "image:test",
			StartCommand: "echo bootstrap",
		}
	}
	defer func() { runpodDesiredTemplateSpec = origSpec }()

	var buf bytes.Buffer
	runpodTemplatePrintBootstrapCmd.SetOut(&buf)
	runpodTemplatePrintBootstrapCmd.SetErr(&buf)

	if err := runRunpodTemplatePrintBootstrap(runpodTemplatePrintBootstrapCmd, nil); err != nil {
		t.Fatalf("runRunpodTemplatePrintBootstrap: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Image: image:test", "WEFT_BOOTSTRAP_KEY", "Startup command:", "echo bootstrap"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRunpodDiagnosisIncludesChecks(t *testing.T) {
	var buf bytes.Buffer
	printRunpodDiagnosis(&buf, &runpod.Diagnosis{
		SearchReady:          true,
		LaunchReady:          true,
		DefaultImage:         "image:test",
		RequiredStartCommand: "echo hi",
		SearchChecks:         []runpod.Check{{Name: "auth", OK: true, Detail: "authenticated"}},
		LaunchChecks:         []runpod.Check{{Name: "template compatibility", OK: true, Detail: "matches"}},
	})

	for _, want := range []string{"[OK] auth: authenticated", "[OK] template compatibility: matches"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("diagnosis missing %q:\n%s", want, buf.String())
		}
	}
}
