package main

import (
	"errors"
	"os"

	"github.com/osteele/weft/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		var gateErr *cmd.EdgeGateError
		if errors.As(err, &gateErr) {
			os.Exit(gateErr.ExitCode)
		}
		os.Exit(1)
	}
}
