package main

import (
	"os"

	"github.com/osteele/weft/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
