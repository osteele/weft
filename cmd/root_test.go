package cmd

import (
	"errors"
	"testing"
)

func TestIsUsageError_RequiredFlag(t *testing.T) {
	err := errors.New(`required flag(s) "older-than" not set`)
	if !isUsageError(err) {
		t.Fatalf("expected required flag error to be treated as usage")
	}
}
