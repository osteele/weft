package sync

import "testing"

func TestParseJjInfo(t *testing.T) {
	tests := []struct {
		name   string
		output string
		dirty  bool
		valid  bool
	}{
		{name: "dirty", output: "change-id\ncommit-id\nfalse\n", dirty: true, valid: true},
		{name: "clean", output: "change-id\ncommit-id\ntrue\n", dirty: false, valid: true},
		{name: "old output", output: "change-id\ncommit-id\n", valid: false},
		{name: "invalid empty", output: "change-id\ncommit-id\nunknown\n", valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := parseJjInfo(tc.output)
			if !tc.valid {
				if info != nil {
					t.Fatalf("parseJjInfo = %+v, want nil", info)
				}
				return
			}
			if info == nil || info.ChangeID != "change-id" || info.Revision != "commit-id" || info.Dirty != tc.dirty {
				t.Fatalf("parseJjInfo = %+v, want dirty=%v", info, tc.dirty)
			}
		})
	}
}
