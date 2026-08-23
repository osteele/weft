package runner

import (
	"math/rand"
	"os"
	"testing"
)

const gibKB = int64(1024 * 1024)

func TestEvaluateRAMAdmission(t *testing.T) {
	tests := []struct {
		name        string
		input       RAMAdmissionInput
		admit       bool
		projectedGB int64
	}{
		{
			name: "accounts for unmaterialized reservation",
			input: RAMAdmissionInput{TotalKB: 64 * gibKB, UsedKB: 8 * gibKB, TargetPercent: 90,
				Running: []RAMCommitment{{ReservationKB: 32 * gibKB}}, NextReservationKB: 16 * gibKB},
			admit: true, projectedGB: 56,
		},
		{
			name: "blocks concurrent set above target",
			input: RAMAdmissionInput{TotalKB: 64 * gibKB, UsedKB: 36 * gibKB, TargetPercent: 90,
				Running: []RAMCommitment{{ReservationKB: 32 * gibKB, CurrentRSSKB: 28 * gibKB}}, NextReservationKB: 24 * gibKB},
			admit: false, projectedGB: 64,
		},
		{
			name: "does not double count resident memory",
			input: RAMAdmissionInput{TotalKB: 64 * gibKB, UsedKB: 40 * gibKB, TargetPercent: 90,
				Running: []RAMCommitment{{ReservationKB: 32 * gibKB, CurrentRSSKB: 32 * gibKB}}, NextReservationKB: 16 * gibKB},
			admit: true, projectedGB: 56,
		},
		{
			name:  "unknown host memory fails open",
			input: RAMAdmissionInput{NextReservationKB: 100 * gibKB},
			admit: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateRAMAdmission(test.input)
			if got.Admit != test.admit {
				t.Fatalf("Admit = %v, projected=%d capacity=%d, want %v", got.Admit, got.ProjectedUsedKB, got.CapacityKB, test.admit)
			}
			if test.projectedGB > 0 && got.ProjectedUsedKB != test.projectedGB*gibKB {
				t.Fatalf("ProjectedUsedKB = %d, want %d GiB", got.ProjectedUsedKB, test.projectedGB)
			}
		})
	}
}

type ramSimulation struct {
	totalKB   int64
	foreignKB int64
	running   map[string]RAMCommitment
}

func newRAMSimulation(totalGB, foreignGB int64) *ramSimulation {
	return &ramSimulation{totalKB: totalGB * gibKB, foreignKB: foreignGB * gibKB, running: make(map[string]RAMCommitment)}
}

func (s *ramSimulation) tryStart(name string, reservationGB int64) bool {
	input := RAMAdmissionInput{TotalKB: s.totalKB, UsedKB: s.usedKB(), TargetPercent: 90, NextReservationKB: reservationGB * gibKB}
	for _, commitment := range s.running {
		input.Running = append(input.Running, commitment)
	}
	decision := EvaluateRAMAdmission(input)
	if decision.Admit {
		s.running[name] = RAMCommitment{ReservationKB: reservationGB * gibKB}
	}
	return decision.Admit
}

func (s *ramSimulation) grow(name string, rssGB int64) {
	commitment := s.running[name]
	commitment.CurrentRSSKB = rssGB * gibKB
	s.running[name] = commitment
}

func (s *ramSimulation) finish(name string) { delete(s.running, name) }

func (s *ramSimulation) usedKB() int64 {
	used := s.foreignKB
	for _, commitment := range s.running {
		used += commitment.CurrentRSSKB
	}
	return used
}

func TestRAMAdmissionEventSimulation(t *testing.T) {
	sim := newRAMSimulation(64, 8)
	if !sim.tryStart("model-a", 32) {
		t.Fatal("model-a should start")
	}
	sim.grow("model-a", 28)
	if sim.tryStart("model-b", 24) {
		t.Fatal("model-b should wait while model-a retains its reservation")
	}
	sim.finish("model-a")
	if !sim.tryStart("model-b", 24) {
		t.Fatal("model-b should start after model-a releases its reservation")
	}
	sim.grow("model-b", 24)
	sim.foreignKB = 30 * gibKB
	if sim.tryStart("small", 4) {
		t.Fatal("foreign memory growth should close admission")
	}
}

func TestRAMAdmissionSeededMonotonicity(t *testing.T) {
	rng := rand.New(rand.NewSource(20260823))
	for i := 0; i < 10_000; i++ {
		input := RAMAdmissionInput{
			TotalKB:           int64(rng.Intn(1023)+1) * gibKB,
			UsedKB:            int64(rng.Intn(1024)) * gibKB,
			TargetPercent:     rng.Intn(101),
			NextReservationKB: int64(rng.Intn(256)) * gibKB,
		}
		larger := input
		larger.NextReservationKB += int64(rng.Intn(256)) * gibKB
		if !EvaluateRAMAdmission(input).Admit && EvaluateRAMAdmission(larger).Admit {
			t.Fatalf("larger reservation became admissible: input=%+v larger=%+v", input, larger)
		}
	}
}

func FuzzRAMAdmissionReservationMonotonicity(f *testing.F) {
	f.Add(int64(64), int64(8), int64(16), int64(8))
	f.Fuzz(func(t *testing.T, totalGB, usedGB, reservationGB, incrementGB int64) {
		if totalGB <= 0 || totalGB > 4096 || usedGB < 0 || usedGB > 8192 || reservationGB < 0 || reservationGB > 4096 || incrementGB < 0 || incrementGB > 4096 {
			t.Skip()
		}
		base := RAMAdmissionInput{TotalKB: totalGB * gibKB, UsedKB: usedGB * gibKB, TargetPercent: 90, NextReservationKB: reservationGB * gibKB}
		larger := base
		larger.NextReservationKB += incrementGB * gibKB
		if !EvaluateRAMAdmission(base).Admit && EvaluateRAMAdmission(larger).Admit {
			t.Fatal("increasing a reservation changed reject to admit")
		}
	})
}

func TestProcCurrentRSSKBCurrentProcess(t *testing.T) {
	if got := ProcCurrentRSSKB(os.Getpid()); got <= 0 {
		t.Skip("process RSS probe unavailable in this test environment")
	}
}
