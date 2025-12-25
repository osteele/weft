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

	blockHostPlan := &File{
		Version: 1,
		Jobs: []Entry{
			{Parallel: &Parallel{
				Host: "block-host",
				Jobs: []Job{{Command: "c"}},
			}},
			{Series: &Series{
				Host: "series-host",
				Jobs: []Job{{Command: "a"}, {Host: "override", Command: "b"}},
			}},
		},
	}
	if err := blockHostPlan.ApplyDefaults(Defaults{}); err != nil {
		t.Fatalf("expected block host defaults to apply: %v", err)
	}
	if got := blockHostPlan.Jobs[0].Parallel.Jobs[0].Host; got != "block-host" {
		t.Fatalf("expected parallel job host to inherit block host, got %q", got)
	}
	if got := blockHostPlan.Jobs[1].Series.Jobs[0].Host; got != "series-host" {
		t.Fatalf("expected series job host to inherit block host, got %q", got)
	}
	if got := blockHostPlan.Jobs[1].Series.Jobs[1].Host; got != "override" {
		t.Fatalf("expected explicit job host to override block host, got %q", got)
	}
}

func TestBuildExecutionPlan(t *testing.T) {
	pf := &File{
		Version: 1,
		Jobs: []Entry{
			{Parallel: &Parallel{
				ID:   "extract",
				Name: "extract",
				Jobs: []Job{
					{ID: "extract.a", Host: "h1", Command: "cmd-a"},
					{Host: "h1", Command: "cmd-b"},
				},
			}},
			{Parallel: &Parallel{
				ID:                "train",
				DependsOn:         []string{"extract"},
				Jobs:              []Job{{ID: "train.a", Host: "h1", Command: "train", DependsOn: []string{"extract.a"}}},
				ContinueOnFailure: true,
			}},
		},
	}
	if err := pf.Validate(); err != nil {
		t.Fatalf("expected base plan to validate: %v", err)
	}
	plan, err := pf.BuildExecutionPlan()
	if err != nil {
		t.Fatalf("build execution plan: %v", err)
	}
	if len(plan.Jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(plan.Jobs))
	}
	if plan.Jobs[0].ID != "extract.a" {
		t.Fatalf("expected first job to be extract.a, got %s", plan.Jobs[0].ID)
	}
	if len(plan.Jobs[2].Dependencies) == 0 {
		t.Fatalf("expected train job to have dependencies")
	}
	found := false
	for _, dep := range plan.Jobs[2].Dependencies {
		if dep.Job.ID == "extract.a" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("train job missing dependency on extract.a")
	}
}

func TestBuildExecutionPlanUnknownDependency(t *testing.T) {
	pf := &File{
		Version: 1,
		Jobs: []Entry{
			{Job: &Job{
				ID:        "job1",
				Host:      "h1",
				Command:   "cmd",
				DependsOn: []string{"missing"},
			}},
		},
	}
	if err := pf.Validate(); err != nil {
		t.Fatalf("expected plan to validate: %v", err)
	}
	if _, err := pf.BuildExecutionPlan(); err == nil {
		t.Fatalf("expected execution plan to fail for unknown dependency")
	}
}
