package campaign

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/config"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "weft-campaign-config-*")
	if err != nil {
		panic(err)
	}
	restore := config.SetConfigPathsForTesting(
		filepath.Join(dir, "config.toml"),
		filepath.Join(dir, "config.yaml"),
	)
	code := m.Run()
	restore()
	os.RemoveAll(dir)
	os.Exit(code)
}
