package cmd

import (
	"os"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) {
	original := ensurePredictorUsableFunc
	originalLoadConfig := loadPredictorConfig
	ensurePredictorUsableFunc = func(*cobra.Command, *config.Config, string) error {
		return nil
	}
	loadPredictorConfig = func() (*config.Config, error) {
		return nil, nil
	}
	code := m.Run()
	ensurePredictorUsableFunc = original
	loadPredictorConfig = originalLoadConfig
	os.Exit(code)
}
