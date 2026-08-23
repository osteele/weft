package runner

import "testing"

func TestApplyCPUObservationRespectsWarmupAndCopiesState(t *testing.T) {
	cfg := DefaultCPUConfig()
	original := CPUAllotmentState{
		WarmupUntil:    100,
		LocalAllotment: 40,
		Samples:        []int{10, 20},
		OverHist:       []int{1},
	}
	next, changed := cfg.ApplyCPUObservation(original, 99, 90, 40)
	if changed || len(next.Samples) != 2 {
		t.Fatalf("pre-warmup observation changed state: %+v", next)
	}

	next, _ = cfg.ApplyCPUObservation(original, 100, 30, 40)
	next.Samples[0] = 999
	if original.Samples[0] != 10 {
		t.Fatal("ApplyCPUObservation mutated its input slices")
	}
}

type cpuSimulation struct {
	cfg  CPUConfig
	now  int64
	jobs map[string]CPUAllotmentState
}

func newCPUSimulation() *cpuSimulation {
	return &cpuSimulation{cfg: DefaultCPUConfig(), jobs: make(map[string]CPUAllotmentState)}
}

func (s *cpuSimulation) start(name string, allotment int) {
	s.jobs[name] = CPUAllotmentState{
		WarmupUntil:    s.now + int64(s.cfg.WarmupDuration),
		LocalAllotment: allotment,
	}
}

func (s *cpuSimulation) totalAllotment() int {
	total := 0
	for _, job := range s.jobs {
		total += job.LocalAllotment
	}
	return total
}

func (s *cpuSimulation) observe(name string, hostPct int) {
	s.now += int64(s.cfg.SampleInterval)
	state := s.jobs[name]
	next, _ := s.cfg.ApplyCPUObservation(state, s.now, hostPct, s.totalAllotment())
	s.jobs[name] = next
}

func (s *cpuSimulation) canStart(allotment int) bool {
	return s.totalAllotment()+allotment <= s.cfg.HostUtilizationTarget
}

func TestAdaptiveCPUEventSimulationReleasesCapacity(t *testing.T) {
	sim := newCPUSimulation()
	sim.start("loader", 50)
	if sim.canStart(40) {
		t.Fatal("second job should not fit before observation")
	}

	for sim.now < int64(sim.cfg.WarmupDuration) {
		sim.observe("loader", 5)
	}
	if sim.jobs["loader"].LocalAllotment != 50 {
		t.Fatal("warmup samples must not change the allotment")
	}
	for i := 0; i < sim.cfg.SampleCount()+sim.cfg.HysteresisThreshold-1; i++ {
		sim.observe("loader", 5)
	}
	if got := sim.jobs["loader"].LocalAllotment; got != 40 {
		t.Fatalf("allotment after persistent under-use = %d, want 40", got)
	}
	if !sim.canStart(40) {
		t.Fatal("decay should release enough CPU capacity for the second job")
	}
}

func TestAdaptiveCPUEventSimulationCapsGrowthAtHostTarget(t *testing.T) {
	sim := newCPUSimulation()
	sim.now = 1_000
	sim.jobs["busy"] = CPUAllotmentState{LocalAllotment: 20}
	sim.jobs["peer"] = CPUAllotmentState{LocalAllotment: 30}
	for i := 0; i < sim.cfg.SampleCount()+sim.cfg.HysteresisThreshold-1; i++ {
		sim.observe("busy", 70)
	}
	if got := sim.jobs["busy"].LocalAllotment; got != 50 {
		t.Fatalf("busy allotment = %d, want 50 headroom cap", got)
	}
	if got := sim.totalAllotment(); got != sim.cfg.HostUtilizationTarget {
		t.Fatalf("total allotment = %d, want %d", got, sim.cfg.HostUtilizationTarget)
	}
}

func FuzzApplyCPUObservationBounds(f *testing.F) {
	f.Add(40, 20, 40, 50)
	f.Fuzz(func(t *testing.T, allotment, hostPct, totalAllotment, priorDecision int) {
		cfg := DefaultCPUConfig()
		if allotment < cfg.MinAllotment || allotment > cfg.MaxAllotment || hostPct < 0 || hostPct > 100 || totalAllotment < allotment || totalAllotment > cfg.HostUtilizationTarget {
			t.Skip()
		}
		decision := priorDecision & 1
		state := CPUAllotmentState{
			LocalAllotment: allotment,
			Samples:        make([]int, cfg.SampleCount()-1),
			OverHist:       []int{decision, decision},
			UnderHist:      []int{1 - decision, 1 - decision},
		}
		next, _ := cfg.ApplyCPUObservation(state, 1, hostPct, totalAllotment)
		if next.LocalAllotment < cfg.MinAllotment || next.LocalAllotment > cfg.MaxAllotment {
			t.Fatalf("allotment %d outside [%d,%d]", next.LocalAllotment, cfg.MinAllotment, cfg.MaxAllotment)
		}
		if next.LocalAllotment > allotment && totalAllotment-allotment+next.LocalAllotment > cfg.HostUtilizationTarget {
			t.Fatalf("increase exceeds host target: old=%d next=%d total=%d", allotment, next.LocalAllotment, totalAllotment)
		}
	})
}
