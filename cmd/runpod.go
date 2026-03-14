package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/runpod"
	"github.com/spf13/cobra"
)

var (
	runpodDiagnose            = runpod.Diagnose
	runpodSetup               = runpod.Setup
	runpodDesiredTemplateSpec = runpod.DesiredBootstrapTemplate
)

var runpodCmd = &cobra.Command{
	Use:   "runpod",
	Short: "Inspect and configure the RunPod integration",
}

var runpodDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check RunPod CLI, auth, config, and template readiness",
	Args:  cobra.NoArgs,
	RunE:  runRunpodDoctor,
}

var runpodSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Enable RunPod, ensure a bootstrap template exists, and persist config",
	Args:  cobra.NoArgs,
	RunE:  runRunpodSetup,
}

var runpodTemplateCmd = &cobra.Command{
	Use:   "template",
	Short: "Inspect the RunPod bootstrap template requirements",
}

var runpodTemplatePrintBootstrapCmd = &cobra.Command{
	Use:   "print-bootstrap",
	Short: "Print the exact bootstrap startup command and required env vars",
	Args:  cobra.NoArgs,
	RunE:  runRunpodTemplatePrintBootstrap,
}

func init() {
	rootCmd.AddCommand(runpodCmd)
	runpodCmd.AddCommand(runpodDoctorCmd)
	runpodCmd.AddCommand(runpodSetupCmd)
	runpodCmd.AddCommand(runpodTemplateCmd)
	runpodTemplateCmd.AddCommand(runpodTemplatePrintBootstrapCmd)
}

func runRunpodDoctor(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	diag, err := runpodDiagnose(cfg)
	if err != nil {
		return err
	}
	printRunpodDiagnosis(cmd.OutOrStdout(), diag)
	if !diag.LaunchReady {
		return fmt.Errorf("RunPod is not launch-ready")
	}
	return nil
}

func runRunpodSetup(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	result, err := runpodSetup(cfg)
	if result != nil && result.Diagnosis != nil {
		printRunpodDiagnosis(cmd.OutOrStdout(), result.Diagnosis)
	}
	if result != nil {
		if result.Template != nil {
			action := "reused"
			if result.CreatedTemplate {
				action = "created"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nTemplate %s: %s (%s)\n", action, result.Template.ID, result.Template.Name)
		}
		if result.UpdatedConfig {
			fmt.Fprintf(cmd.OutOrStdout(), "Updated config: %s\n", result.ConfigPath)
		}
	}
	return err
}

func runRunpodTemplatePrintBootstrap(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	spec := runpodDesiredTemplateSpec(cfg)
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Image: %s\n", spec.Image)
	fmt.Fprintln(w, "Required env vars:")
	for _, env := range []string{
		"R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY",
		"R2_ENDPOINT",
		"R2_BUCKET",
		"WEFT_BOOTSTRAP_KEY",
	} {
		fmt.Fprintf(w, "  %s\n", env)
	}
	fmt.Fprintln(w, "Recommended template settings:")
	fmt.Fprintln(w, "  - user template")
	fmt.Fprintln(w, "  - startup command below")
	fmt.Fprintln(w, "  - SSH left enabled on pods")
	fmt.Fprintln(w, "Startup command:")
	fmt.Fprintln(w, spec.StartCommand)
	return nil
}

func printRunpodDiagnosis(w io.Writer, diag *runpod.Diagnosis) {
	if diag == nil {
		return
	}
	fmt.Fprintln(w, "RunPod readiness")
	fmt.Fprintf(w, "  Search ready: %s\n", yesNo(diag.SearchReady))
	fmt.Fprintf(w, "  Launch ready: %s\n", yesNo(diag.LaunchReady))
	if diag.CLIPath != "" {
		fmt.Fprintf(w, "  CLI: %s (%s)\n", diag.Version, diag.CLIPath)
	}
	if diag.SearchCommand != "" {
		fmt.Fprintf(w, "  Search command: %s\n", diag.SearchCommand)
	}
	if diag.PodCommandFamily != "" {
		fmt.Fprintf(w, "  Pod commands: %s\n", diag.PodCommandFamily)
	}
	if diag.TemplateCommandFamily != "" {
		fmt.Fprintf(w, "  Template commands: %s\n", diag.TemplateCommandFamily)
	}
	fmt.Fprintf(w, "  Template image: %s\n", diag.DefaultImage)
	fmt.Fprintf(w, "  bootstrap_template_id: %s\n", valueOrUnset(diag.TemplateID))
	if diag.Template != nil {
		fmt.Fprintf(w, "  Template: %s (%s)\n", diag.Template.ID, diag.Template.Name)
	}
	fmt.Fprintln(w, "\nSearch checks:")
	for _, check := range diag.SearchChecks {
		printRunpodCheck(w, check)
	}
	fmt.Fprintln(w, "Launch checks:")
	for _, check := range diag.LaunchChecks {
		printRunpodCheck(w, check)
	}
	fmt.Fprintln(w, "Required startup command:")
	fmt.Fprintln(w, diag.RequiredStartCommand)
}

func printRunpodCheck(w io.Writer, check runpod.Check) {
	status := "FAIL"
	if check.OK {
		status = "OK"
	}
	detail := strings.TrimSpace(check.Detail)
	if detail == "" {
		fmt.Fprintf(w, "  [%s] %s\n", status, check.Name)
		return
	}
	fmt.Fprintf(w, "  [%s] %s: %s\n", status, check.Name, detail)
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func valueOrUnset(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(unset)"
	}
	return value
}
