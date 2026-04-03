package cmd

import (
	"os"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) {
	original := ensurePredictorUsableFunc
	ensurePredictorUsableFunc = func(*cobra.Command, *config.Config, string) error {
		return nil
	}
	code := m.Run()
	ensurePredictorUsableFunc = original
	os.Exit(code)
}
