package cmd

import (
	"bytes"
	"fmt"
	"os/exec"

	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

var estimationEvalJSON bool
var estimationEvalMinFamilySize int

var estimationEvalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Evaluate predictor accuracy against configured training data",
	RunE:  runEstimationEval,
}

func init() {
	estimationCmd.AddCommand(estimationEvalCmd)
	estimationEvalCmd.Flags().BoolVar(&estimationEvalJSON, "json", false, "Emit machine-readable JSON")
	estimationEvalCmd.Flags().IntVar(&estimationEvalMinFamilySize, "min-family-size", 5, "Minimum script-family sample count to report separately")
}

func runEstimationEval(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pcfg := buildPredictorConfig(cfg)
	if !pcfg.Configured() {
		return fmt.Errorf("predictor not configured: set predictor.project_path in %s", config.ConfigPath())
	}

	cliArgs := []string{
		"run",
		"--project", pcfg.ProjectPath,
		"job-estimator",
		"eval",
		"--model-dir", pcfg.ModelDirPath(),
	}
	for _, dbPath := range pcfg.DBPaths {
		cliArgs = append(cliArgs, "--db", dbPath)
	}
	if estimationEvalJSON {
		cliArgs = append(cliArgs, "--json")
	}
	if estimationEvalMinFamilySize > 0 {
		cliArgs = append(cliArgs, "--min-family-size", fmt.Sprintf("%d", estimationEvalMinFamilySize))
	}
	estimator := exec.Command("uv", cliArgs...)
	var stderr bytes.Buffer
	estimator.Stderr = &stderr
	out, err := estimator.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("estimator eval: %w: %s", err, stderr.String())
		}
		return fmt.Errorf("estimator eval: %w", err)
	}
	_, err = cmd.OutOrStdout().Write(out)
	return err
}
