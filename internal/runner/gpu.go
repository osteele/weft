package runner

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
)

// GPUInfo describes a single GPU device.
type GPUInfo struct {
	Index      string
	Name       string
	TotalMemGB int
}

// DeviceMemInfo holds actual GPU memory usage for a single device.
type DeviceMemInfo struct {
	UsedMiB  int
	TotalMiB int
}

// GPUInventory holds discovered GPU information.
type GPUInventory struct {
	Devices           []GPUInfo
	DeviceMemSnapshot map[string]DeviceMemInfo // actual per-device memory, refreshed each tick
	hasNvidiaSmi      bool                     // cached LookPath result from DiscoverGPUs
}

// parseNvidiaSmiOutput parses nvidia-smi CSV output into rows of trimmed fields.
// Each line is split into at most fieldCount fields using ", " as delimiter.
// Lines with fewer than fieldCount fields are skipped.
func parseNvidiaSmiOutput(out string, fieldCount int) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ", ", fieldCount)
		if len(parts) < fieldCount {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		rows = append(rows, parts)
	}
	return rows
}

// queryNvidiaSmi runs nvidia-smi with the given query flag and parses the output.
// Returns nil if nvidia-smi is not available or the command fails.
func queryNvidiaSmi(queryFlag string, fieldCount int) [][]string {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil
	}
	return execNvidiaSmi(queryFlag, fieldCount)
}

// queryNvidiaSmiCached runs nvidia-smi using the cached LookPath result.
func (inv *GPUInventory) queryNvidiaSmiCached(queryFlag string, fieldCount int) [][]string {
	if !inv.hasNvidiaSmi {
		return nil
	}
	return execNvidiaSmi(queryFlag, fieldCount)
}

// execNvidiaSmi runs nvidia-smi and parses the output (no LookPath check).
func execNvidiaSmi(queryFlag string, fieldCount int) [][]string {
	out, err := exec.Command("nvidia-smi",
		queryFlag,
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSmiOutput(string(out), fieldCount)
}

// DiscoverGPUs runs nvidia-smi to discover available GPU devices.
// Returns an empty inventory if nvidia-smi is not available.
// Populates both Devices and an initial DeviceMemSnapshot in a single query.
func DiscoverGPUs() *GPUInventory {
	inv := &GPUInventory{}
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return inv
	}
	inv.hasNvidiaSmi = true

	rows := inv.queryNvidiaSmiCached("--query-gpu=index,name,memory.used,memory.total", 4)
	if rows == nil {
		return inv
	}

	inv.DeviceMemSnapshot = make(map[string]DeviceMemInfo, len(rows))
	for _, parts := range rows {
		idx := parts[0]
		name := parts[1]
		usedMiB, _ := strconv.Atoi(parts[2])
		totalMiB, _ := strconv.Atoi(parts[3])
		memGB := totalMiB / 1024

		inv.Devices = append(inv.Devices, GPUInfo{
			Index:      idx,
			Name:       name,
			TotalMemGB: memGB,
		})
		inv.DeviceMemSnapshot[idx] = DeviceMemInfo{UsedMiB: usedMiB, TotalMiB: totalMiB}
	}

	return inv
}

// RefreshDeviceMemSnapshot queries nvidia-smi for actual per-device memory usage
// and stores the result in DeviceMemSnapshot. Safe to call on non-GPU hosts
// (sets an empty map).
func (inv *GPUInventory) RefreshDeviceMemSnapshot() {
	result := make(map[string]DeviceMemInfo)
	rows := inv.queryNvidiaSmiCached("--query-gpu=index,memory.used,memory.total", 3)
	for _, parts := range rows {
		idx := parts[0]
		used, _ := strconv.Atoi(parts[1])
		total, _ := strconv.Atoi(parts[2])
		result[idx] = DeviceMemInfo{UsedMiB: used, TotalMiB: total}
	}
	inv.DeviceMemSnapshot = result
}

// PerDeviceGPUMemUsedMiB queries nvidia-smi for actual per-device memory usage.
// Returns an empty map if nvidia-smi is unavailable.
func PerDeviceGPUMemUsedMiB() map[string]DeviceMemInfo {
	result := make(map[string]DeviceMemInfo)
	rows := queryNvidiaSmi("--query-gpu=index,memory.used,memory.total", 3)
	for _, parts := range rows {
		idx := parts[0]
		used, _ := strconv.Atoi(parts[1])
		total, _ := strconv.Atoi(parts[2])
		result[idx] = DeviceMemInfo{UsedMiB: used, TotalMiB: total}
	}
	return result
}

// LogInventory logs the discovered GPU devices for diagnostics.
func (inv *GPUInventory) LogInventory() {
	if len(inv.Devices) == 0 {
		fmt.Println("GPU inventory: no devices discovered")
		return
	}
	fmt.Printf("GPU inventory: %d devices\n", len(inv.Devices))
	for _, d := range inv.Devices {
		fmt.Printf("  [%s] %s (%dGB)\n", d.Index, d.Name, d.TotalMemGB)
	}
}

// DevicesByClass returns device indices matching a GPU class name.
// Supports exact model names (e.g. "a100"), generation names (e.g. "ampere"),
// and minimum generation constraints (e.g. "ampere+").
func (inv *GPUInventory) DevicesByClass(className string) []string {
	constraint := placement.ParseGPUConstraint(className)
	var result []string
	for _, d := range inv.Devices {
		if constraint.MatchesGPUFullName(d.Name) {
			result = append(result, d.Index)
		}
	}
	return result
}

// TotalMemGB returns the total memory in GB for a device index.
func (inv *GPUInventory) TotalMemGB(deviceIdx string) int {
	for _, d := range inv.Devices {
		if d.Index == deviceIdx {
			return d.TotalMemGB
		}
	}
	return 0
}

// DeviceHasRunningJob checks if any running job is using a specific GPU device.
func DeviceHasRunningJob(state *State, device string) bool {
	for _, rs := range state.Running {
		for _, d := range rs.GPUDevices {
			if d == device {
				return true
			}
		}
	}
	return false
}

// TotalGPUMemReserved returns the total GPU memory reserved by running jobs on a device.
func TotalGPUMemReserved(state *State, device string) int {
	total := 0
	for _, rs := range state.Running {
		for _, d := range rs.GPUDevices {
			if d == device {
				total += rs.GPUMemGB
				break
			}
		}
	}
	return total
}

// deviceMemCheck checks whether a device can accommodate memRequiredGB of GPU memory,
// considering both our own reservations and actual VRAM usage from the snapshot.
// Returns the effective free memory in MiB (for best-device selection) and whether
// the device passes both checks.
func (inv *GPUInventory) deviceMemCheck(state *State, device string, memRequiredGB int) (freeMiB int, ok bool) {
	totalMem := inv.TotalMemGB(device)
	reserved := TotalGPUMemReserved(state, device)
	if memRequiredGB > 0 && memRequiredGB > totalMem-reserved {
		return 0, false
	}

	// Check actual VRAM usage if snapshot is available
	freeMiB = totalMem * 1024 // fallback: use static total in MiB
	if info, found := inv.DeviceMemSnapshot[device]; found {
		freeMiB = info.TotalMiB - info.UsedMiB
		memRequiredMiB := memRequiredGB * 1024
		if memRequiredMiB > freeMiB {
			return 0, false
		}
	}
	return freeMiB, true
}

// PickBestGPUForClass finds the best available GPU device for a given class.
// Returns the device index and true, or "" and false if none available.
// Checks both our own reservations and actual VRAM usage (from DeviceMemSnapshot)
// to avoid placing jobs on devices where other users have consumed memory.
func (inv *GPUInventory) PickBestGPUForClass(state *State, className string, memRequired int) (string, bool) {
	candidates := inv.DevicesByClass(className)
	if len(candidates) == 0 {
		return "", false
	}

	bestDevice := ""
	bestFreeMiB := -1

	for _, device := range candidates {
		if DeviceHasRunningJob(state, device) {
			continue
		}
		freeMiB, ok := inv.deviceMemCheck(state, device, memRequired)
		if !ok {
			continue
		}
		if freeMiB > bestFreeMiB {
			bestFreeMiB = freeMiB
			bestDevice = device
		}
	}

	if bestDevice == "" {
		return "", false
	}
	fmt.Printf("  GPU class '%s': selected device %s (%d MiB free)\n", className, bestDevice, bestFreeMiB)
	return bestDevice, true
}

// CanStartGPUJob checks if a job can start based on GPU constraints.
// Returns true if the job can start, along with the resolved GPU devices.
// Checks both our own reservations and actual VRAM usage (from DeviceMemSnapshot).
func (inv *GPUInventory) CanStartGPUJob(state *State, job *RunnerJob) (bool, []string) {
	// GPU class-based job
	if job.Data.GPUClass != "" {
		memPerDevice := GetJobGPUMem(job.Data, DefaultGPUMemGB)
		device, ok := inv.PickBestGPUForClass(state, job.Data.GPUClass, memPerDevice)
		if !ok {
			return false, nil
		}
		return true, []string{device}
	}

	devices := GetJobGPUDevices(job.Data)
	if len(devices) == 0 {
		// CPU-only job — always allowed
		return true, nil
	}

	memPerDevice := GetJobGPUMem(job.Data, DefaultGPUMemGB)
	for _, device := range devices {
		if DeviceHasRunningJob(state, device) {
			return false, nil
		}
		if _, ok := inv.deviceMemCheck(state, device, memPerDevice); !ok {
			return false, nil
		}
	}
	return true, devices
}

// DefaultGPUMemGB is the default GPU memory reservation when a job uses a GPU.
const DefaultGPUMemGB = 20

// RunnerJob wraps a CommandJob with runtime metadata.
type RunnerJob struct {
	Data *ops.CommandJob
	ID   int64
}

// QueryGPUProcessMemory queries nvidia-smi for GPU memory usage by process PIDs.
// Returns total GPU memory in MiB used by any of the given PIDs.
func QueryGPUProcessMemory(pids []int) int {
	rows := queryNvidiaSmi("--query-compute-apps=pid,used_memory", 2)
	if rows == nil {
		return 0
	}

	pidSet := make(map[int]bool, len(pids))
	for _, p := range pids {
		pidSet[p] = true
	}

	total := 0
	for _, parts := range rows {
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		if pidSet[pid] {
			mem, _ := strconv.Atoi(parts[1])
			total += mem
		}
	}
	return total
}

// FormatGPUDeviceEnv creates a CUDA_VISIBLE_DEVICES=... string for the given devices.
func FormatGPUDeviceEnv(devices []string) string {
	if len(devices) == 0 {
		return ""
	}
	return fmt.Sprintf("CUDA_VISIBLE_DEVICES=%s", strings.Join(devices, ","))
}
