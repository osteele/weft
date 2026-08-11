package db

import "testing"

func TestRequestedMaxHourlyRateCentsForJobs(t *testing.T) {
	capA := 3320
	capB := 3000
	jobs := []*Job{
		{CLIResourceOverrides: &CLIResourceOverrides{MaxHourlyRateCents: &capA}},
		{CLIResourceOverrides: &CLIResourceOverrides{MaxHourlyRateCents: &capB}},
	}
	cap, allExplicit := RequestedMaxHourlyRateCentsForJobs(jobs)
	if cap != capB || !allExplicit {
		t.Fatalf("cap = %d, allExplicit = %v; want %d, true", cap, allExplicit, capB)
	}

	jobs = append(jobs, &Job{})
	cap, allExplicit = RequestedMaxHourlyRateCentsForJobs(jobs)
	if cap != capB || allExplicit {
		t.Fatalf("mixed cap = %d, allExplicit = %v; want %d, false", cap, allExplicit, capB)
	}
}
