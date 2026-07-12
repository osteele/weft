package cmd

import "testing"

func TestInstanceWatchAcceptsOptionalInstanceIDs(t *testing.T) {
	for _, args := range [][]string{
		{"instance", "watch", "wi42"},
		{"instance", "watch", "42"},
		{"watch", "instance", "wi42"},
	} {
		cmd, remaining, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd.RunE == nil {
			t.Fatalf("Find(%v) resolved to command without RunE", args)
		}
		if len(remaining) != 1 || remaining[0] != args[len(args)-1] {
			t.Fatalf("Find(%v) remaining = %v, want [%s]", args, remaining, args[len(args)-1])
		}
	}
}

func TestParseInstanceWatchIDs(t *testing.T) {
	got, err := parseInstanceWatchIDs([]string{"wi42", "43"})
	if err != nil {
		t.Fatalf("parseInstanceWatchIDs: %v", err)
	}
	if len(got) != 2 || got[0] != 42 || got[1] != 43 {
		t.Fatalf("parseInstanceWatchIDs = %v, want [42 43]", got)
	}

	if _, err := parseInstanceWatchIDs([]string{"wj42"}); err == nil {
		t.Fatal("parseInstanceWatchIDs(wj42) = nil error, want invalid instance ID")
	}
}
