package runner

import "math"

const defaultRAMUtilizationTarget = 90

// RAMCommitment describes one running job's reserved and currently resident RAM.
// Values are in KiB, matching the host and process probes.
type RAMCommitment struct {
	ReservationKB int64
	CurrentRSSKB  int64
}

// RAMAdmissionInput is a complete, replayable input to the RAM admission policy.
type RAMAdmissionInput struct {
	TotalKB           int64
	UsedKB            int64
	TargetPercent     int
	Running           []RAMCommitment
	NextReservationKB int64
}

// RAMAdmissionDecision explains the capacity calculation used for one start.
type RAMAdmissionDecision struct {
	Admit                 bool
	CapacityKB            int64
	ProjectedUsedKB       int64
	RemainingCommitmentKB int64
}

// EvaluateRAMAdmission combines live host usage with the not-yet-resident part
// of running reservations. This avoids counting a running job's resident pages
// twice while still protecting against its later growth to the reservation.
func EvaluateRAMAdmission(input RAMAdmissionInput) RAMAdmissionDecision {
	if input.TotalKB <= 0 {
		return RAMAdmissionDecision{Admit: true}
	}
	target := input.TargetPercent
	if target <= 0 || target > 100 {
		target = defaultRAMUtilizationTarget
	}
	capacity := percentOf(input.TotalKB, target)
	remaining := int64(0)
	for _, running := range input.Running {
		reservation := max(running.ReservationKB, 0)
		resident := max(running.CurrentRSSKB, 0)
		if reservation > resident {
			remaining = saturatingAdd(remaining, reservation-resident)
		}
	}
	projected := saturatingAdd(max(input.UsedKB, 0), remaining)
	projected = saturatingAdd(projected, max(input.NextReservationKB, 0))
	return RAMAdmissionDecision{
		Admit:                 projected <= capacity,
		CapacityKB:            capacity,
		ProjectedUsedKB:       projected,
		RemainingCommitmentKB: remaining,
	}
}

func percentOf(value int64, percent int) int64 {
	return (value/100)*int64(percent) + (value%100)*int64(percent)/100
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
