package main

import (
	"testing"
)

func TestCheckDiskCap_Disabled(t *testing.T) {
	// requestedDiskGB=0 means the coordinator didn't populate the field
	// (older coordinator, or local backend). Probe must be a no-op so we
	// don't terminate instances launched by older coordinators.
	if checkDiskCap("", 0, 0, t.TempDir(), "") {
		t.Fatal("checkDiskCap returned true with requested=0; should skip")
	}
}

func TestCheckDiskCap_OK(t *testing.T) {
	// Real filesystem on tmp dir likely has GB free. Request a tiny amount;
	// probe should pass.
	if checkDiskCap("", 0, 1, t.TempDir(), "") {
		t.Fatal("checkDiskCap returned true on healthy filesystem with small request")
	}
}
