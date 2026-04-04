package cmd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/config"
)

func TestIsUsageError_RequiredFlag(t *testing.T) {
	err := errors.New(`required flag(s) "older-than" not set`)
	if !isUsageError(err) {
		t.Fatalf("expected required flag error to be treated as usage")
	}
}

func TestRewriteRootArgs_ExpandsAlias(t *testing.T) {
	cfg := &config.Config{
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft", "uj", "--help"}, cfg)
	want := []string{"weft", "job", "list", "--group-by", "status", "--unprocessed", "--watch", "--help"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}

func TestRewriteRootArgs_ExpandsAliasAfterRootFlag(t *testing.T) {
	cfg := &config.Config{
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft", "--verbose", "uj"}, cfg)
	want := []string{"weft", "--verbose", "job", "list", "--group-by", "status", "--unprocessed", "--watch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}

func TestRewriteRootArgs_AppliesDefaultCommandBeforeAliases(t *testing.T) {
	cfg := &config.Config{
		DefaultCommand: "uj",
		Aliases: map[string]string{
			"uj": "job list --group-by status --unprocessed --watch",
		},
	}

	got := rewriteRootArgs([]string{"weft"}, cfg)
	want := []string{"weft", "job", "list", "--group-by", "status", "--unprocessed", "--watch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteRootArgs() = %v, want %v", got, want)
	}
}
