package placement

import (
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

// GPUReservation is the planning-level view of the GPUs one active
// (non-terminal) attempt holds on its target inventory host. It is derived
// from persisted job state, not tracked in a separate lease table, so it is
// crash-safe by construction: a reservation exists exactly as long as the
// attempt is active, and releases the moment the attempt reaches a terminal
// status.
//
// Devices carries the concrete assigned CUDA device indices once the runner
// has reported them (runner meta file -> status sync ->
// job.Metadata.Resource.GPUDevices). While the job is still queued/starting,
// or when an older agent never reported devices, Devices is empty and Count
// carries the requested device count instead. A job is never counted through
// both fields, so a queued reservation converts to a concrete one without
// double-counting.
type GPUReservation struct {
	JobID    int64
	Devices  []int  // concrete device indices; empty = not yet known
	Count    int    // requested device count, used when Devices is empty
	GPUClass string // reserving job's own class constraint ("" = any GPU)
	GPUMemGB int    // reserving job's per-device memory floor (0 = none)
}

// GPUReservationForJob derives the reservation an active job holds on its
// target host. Returns nil for jobs that hold no GPUs (CPU-only jobs).
func GPUReservationForJob(j *db.Job) *GPUReservation {
	if j == nil {
		return nil
	}
	res := &GPUReservation{JobID: j.ID, GPUClass: j.GPUClass, Count: j.RequestedGPUCount()}
	if j.GPUMemGB != nil && *j.GPUMemGB > 0 {
		res.GPUMemGB = *j.GPUMemGB
	}
	if devices, ok := parseGPUDeviceIndices(j.GPUDevice()); ok {
		res.Devices = devices
		return res
	}
	// No concrete device list. Only GPU-shaped requests reserve capacity by
	// count; this mirrors the runner's gate, which lets jobs without a GPU
	// request start unconditionally (they never occupy a device slot).
	if j.GPUClass == "" && res.GPUMemGB == 0 && res.Count <= 1 {
		return nil
	}
	return res
}

func GPUReservationsForJobs(active []*db.Job) []GPUReservation {
	reservations := make([]GPUReservation, 0, len(active))
	for _, j := range active {
		res := GPUReservationForJob(j)
		if res == nil {
			continue
		}
		reservations = append(reservations, *res)
	}
	return reservations
}

// parseGPUDeviceIndices parses a comma-separated CUDA device list ("0,1").
// Any unparseable entry means the concrete assignment is unknown (older
// agent, non-CUDA device string), so the caller degrades to count-based
// accounting.
func parseGPUDeviceIndices(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	devices := make([]int, 0, len(parts))
	for _, p := range parts {
		idx, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, false
		}
		devices = append(devices, idx)
	}
	return devices, true
}

// FreeGPUCapacity computes how many devices on host match the constraints
// and are not reserved by the given active jobs. It returns the free count
// alongside the total matching count and the reserved count for reporting.
//
// active should be the host's active (non-terminal) inventory-targeted jobs
// (db.ListActiveJobs). A job whose ID equals c.SelfJobID is skipped so that
// re-scoring a host the job is already targeted at does not count the job
// against itself.
//
// This is the planning approximation used by placement eligibility; the
// runner's live GPU gate (occupancy, VRAM, compute-process utilization)
// remains the enforcement point at dispatch time. When reservation data is
// absent — no active jobs, or rows predating device reporting that also
// carry no GPU request — free equals the total matching capacity, so hosts
// without reservation data behave exactly as before.
func FreeGPUCapacity(host inventory.HostSpec, c Constraints, active []*db.Job) (free, matching, reserved int) {
	matchingIdx := make(map[int]struct{})
	anonMatching := 0
	for _, gpu := range host.GPUs {
		if !gpuSpecMatchesConstraints(gpu, c) {
			continue
		}
		if len(gpu.Indices) > 0 {
			for _, idx := range gpu.Indices {
				matchingIdx[idx] = struct{}{}
			}
		} else {
			// Hand-edited host YAML may omit indices; a listed spec is at
			// least one device (mirrors gpuDeviceCount).
			anonMatching++
		}
	}
	matching = len(matchingIdx) + anonMatching
	if matching == 0 {
		return 0, 0, 0
	}

	claimedIdx := make(map[int]struct{})
	countReserved := 0
	for _, j := range active {
		if j == nil || (c.SelfJobID != 0 && j.ID == c.SelfJobID) {
			continue
		}
		res := GPUReservationForJob(j)
		if res == nil {
			continue
		}
		if len(res.Devices) > 0 {
			for _, d := range res.Devices {
				if _, ok := matchingIdx[d]; ok {
					// Deduplicated set: two rows claiming the same device
					// (stale sync data) subtract it once.
					claimedIdx[d] = struct{}{}
				} else if anonMatching > 0 && !hostHasDeviceIndex(host, d) {
					// The assigned index is not recorded anywhere in the
					// host spec (indices omitted), so it cannot be matched
					// by index; count it against the anonymous pool.
					countReserved++
				}
			}
			continue
		}
		// Count-based reservation (devices not yet assigned): it consumes
		// matching capacity only if the reserving job's own request could be
		// satisfied by a device in the matching pool.
		if reservationOverlapsConstraints(host, res, c) {
			countReserved += res.Count
		}
	}
	reserved = len(claimedIdx) + countReserved
	if reserved > matching {
		reserved = matching
	}
	free = matching - reserved
	return free, matching, reserved
}

// reservationOverlapsConstraints reports whether any device on the host
// satisfies both the requesting constraints and the reserving job's own GPU
// request — i.e. whether the two requests compete for the same devices.
func reservationOverlapsConstraints(host inventory.HostSpec, res *GPUReservation, c Constraints) bool {
	rc := Constraints{GPUClass: res.GPUClass, GPUMemGB: res.GPUMemGB}
	for _, gpu := range host.GPUs {
		if gpuSpecMatchesConstraints(gpu, c) && gpuSpecMatchesConstraints(gpu, rc) {
			return true
		}
	}
	return false
}

// gpuSpecMatchesConstraints reports whether a single GPU spec satisfies the
// per-device constraints (class, memory floor, compute-capability range).
func gpuSpecMatchesConstraints(gpu inventory.GPUSpec, c Constraints) bool {
	gc := ParseGPUConstraint(c.GPUClass)
	device := TargetDeviceFromGPU(gpu.Class, gpu.Name, inventory.ParseMemGB(gpu.Memory), gpuDeviceCount(gpu), gpu.Indices)
	return deviceSatisfiesPerDeviceConstraints(device, gc, c, false)
}

func hostHasDeviceIndex(host inventory.HostSpec, idx int) bool {
	for _, gpu := range host.GPUs {
		for _, i := range gpu.Indices {
			if i == idx {
				return true
			}
		}
	}
	return false
}
