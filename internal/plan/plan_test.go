package plan

import "testing"

func TestValidate(t *testing.T) {
	pf := &File{
		Version: 1,
		Jobs: []Entry{
			{Job: &Job{Host: "host", Command: "cmd"}},
		},
	}
	if err := pf.Validate(); err != nil {
		t.Fatalf("expected plan to validate: %v", err)
	}

	missingVersion := &File{Jobs: []Entry{{Job: &Job{Host: "h", Command: "c"}}}}
	if err := missingVersion.Validate(); err == nil {
		t.Fatalf("expected missing version to fail validation")
	}

	whenPlan := &File{
		Version: 1,
		Jobs:    []Entry{{Job: &Job{Host: "h", Command: "c", When: &When{}}}},
	}
	if err := whenPlan.Validate(); err == nil {
		t.Fatalf("expected when block to be rejected")
	}

	seriesBadWait := &File{
		Version: 1,
		Jobs:    []Entry{{Series: &Series{Jobs: []Job{{Host: "h", Command: "c"}}, Wait: "later"}}},
	}
	if err := seriesBadWait.Validate(); err == nil {
		t.Fatalf("expected invalid wait value to fail validation")
	}

	applyDefaultsPlan := &File{
		Version: 1,
		Jobs:    []Entry{{Parallel: &Parallel{Jobs: []Job{{Command: "c1"}, {Host: "h", Command: "c2"}}}}},
	}
	if err := applyDefaultsPlan.ApplyDefaults(Defaults{Host: "default"}); err != nil {
		t.Fatalf("expected defaults to apply: %v", err)
	}
	if applyDefaultsPlan.Jobs[0].Parallel.Jobs[0].Host != "default" {
		t.Fatalf("expected missing host to be filled")
	}
	if err := applyDefaultsPlan.Validate(); err != nil {
		t.Fatalf("expected plan to validate after defaults: %v", err)
	}

	noHostPlan := &File{
		Version: 1,
		Jobs:    []Entry{{Job: &Job{Command: "cmd"}}},
	}
	if err := noHostPlan.ApplyDefaults(Defaults{}); err == nil {
		t.Fatalf("expected error when host missing without default")
	}
}
