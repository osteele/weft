package runner

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
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
	DeviceUtilPct     map[string]int           // per-device GPU utilization %
	DeviceHasCompute  map[string]bool          // true when a compute process is active on device
	isInventoryHost   bool                     // true for on-prem inventory hosts only
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

// nvidiaSmiTableGPULine matches lines like:
//
//	|   0  NVIDIA GeForce RTX 3090  On   | 00000000:01:00.0 Off |     N/A |
var nvidiaSmiTableGPULine = regexp.MustCompile(`\|\s+(\d+)\s+(NVIDIA\s+\S+(?:\s+\S+)*?)\s+(?:On|Off)\s+\|`)

// nvidiaSmiTableMemLine matches lines like:
//
//	| 51%   45C    P8    22W / 350W |      6MiB / 24576MiB |      0%      Default |
var nvidiaSmiTableMemLine = regexp.MustCompile(`\|\s+\d+%\s+\d+C\s+\w+\s+\d+W\s*/\s*\d+W\s*\|\s+(\d+)MiB\s*/\s*(\d+)MiB\s*\|`)

// nvidiaSmiTableUtilLine matches the utilization value in the stats line.
var nvidiaSmiTableUtilLine = regexp.MustCompile(`\|\s+\d+%\s+\d+C\s+\w+\s+\d+W\s*/\s*\d+W\s*\|\s+\d+MiB\s*/\s*\d+MiB\s*\|\s+(\d+)%`)

// nvidiaSmiTableProcessLine matches process rows in the Processes section.
// Example:
// |    0   N/A  N/A      3156      G   /usr/lib/xorg/Xorg                  4MiB |
var nvidiaSmiTableProcessLine = regexp.MustCompile(`^\|\s*(\d+)\s+\S+\s+\S+\s+\d+\s+([CG](?:\+G)?)\s+.+\|$`)

// parseNvidiaSmiTable parses the default nvidia-smi table format that some old
// drivers (e.g. 525.x) return when they silently ignore --query-gpu and --format
// flags. Returns both device info and per-device memory usage in a single pass.
func parseNvidiaSmiTable(out string) ([]GPUInfo, map[string]DeviceMemInfo) {
	lines := strings.Split(out, "\n")

	var devices []GPUInfo
	memSnapshot := make(map[string]DeviceMemInfo)
	for i, line := range lines {
		m := nvidiaSmiTableGPULine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx := m[1]
		name := strings.TrimSpace(m[2])

		// The memory line immediately follows the GPU name line
		if i+1 < len(lines) {
			mm := nvidiaSmiTableMemLine.FindStringSubmatch(lines[i+1])
			if mm != nil {
				usedMiB, _ := strconv.Atoi(mm[1])
				totalMiB, _ := strconv.Atoi(mm[2])
				devices = append(devices, GPUInfo{
					Index:      idx,
					Name:       name,
					TotalMemGB: totalMiB / 1024,
				})
				memSnapshot[idx] = DeviceMemInfo{UsedMiB: usedMiB, TotalMiB: totalMiB}
			}
		}
	}
	return devices, memSnapshot
}

// parseNvidiaSmiTableUtil parses per-device utilization from table output.
func parseNvidiaSmiTableUtil(out string) map[string]int {
	utilByDevice := make(map[string]int)
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		m := nvidiaSmiTableGPULine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx := m[1]
		if i+1 >= len(lines) {
			continue
		}
		if mm := nvidiaSmiTableUtilLine.FindStringSubmatch(lines[i+1]); mm != nil {
			util, _ := strconv.Atoi(mm[1])
			utilByDevice[idx] = util
		}
	}
	return utilByDevice
}

// parseNvidiaSmiTableComputeProcessFlags parses process rows in table output and
// returns a per-device flag for active compute workloads.
func parseNvidiaSmiTableComputeProcessFlags(out string) map[string]bool {
	hasCompute := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		m := nvidiaSmiTableProcessLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		device := m[1]
		procType := m[2]
		// Treat compute-capable process types as active workloads.
		if strings.Contains(procType, "C") {
			hasCompute[device] = true
		}
	}
	return hasCompute
}

// execNvidiaSmiRaw runs nvidia-smi with no extra flags and returns stdout.
func execNvidiaSmiRaw() (string, error) {
	out, err := exec.Command("nvidia-smi").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// DiscoverGPUs runs nvidia-smi to discover available GPU devices.
// Returns an empty inventory if nvidia-smi is not available.
// Populates both Devices and an initial DeviceMemSnapshot in a single query.
// Falls back to parsing the default table format when the CSV query flags
// are silently ignored (observed on driver 525.x).
//
// After discovery, enriches truncated GPU names from the host YAML inventory
// (driver 525.x truncates names to "NVIDIA GeForce ...", breaking class matching).
func DiscoverGPUs() *GPUInventory {
	inv := &GPUInventory{}
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return inv
	}
	inv.hasNvidiaSmi = true
	inv.detectInventoryHost()

	rows := inv.queryNvidiaSmiCached("--query-gpu=index,name,memory.used,memory.total", 4)
	if len(rows) > 0 {
		slog.Debug("nvidia-smi CSV query returned devices", "component", "gpu", "count", len(rows))
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
		inv.enrichFromHostYAML()
		inv.LogInventory()
		return inv
	}

	// Fallback: CSV query returned no rows. Some old drivers (e.g. 525.x)
	// silently ignore --query-gpu/--format and return the default table.
	// Try parsing the table format instead.
	slog.Debug("nvidia-smi CSV query returned 0 rows, trying table format fallback", "component", "gpu")
	tableOut, err := execNvidiaSmiRaw()
	if err != nil {
		slog.Warn("nvidia-smi raw command failed", "component", "gpu", "error", err)
		return inv
	}
	devices, memSnapshot := parseNvidiaSmiTable(tableOut)
	if len(devices) == 0 {
		slog.Debug("table format fallback found 0 devices", "component", "gpu")
		return inv
	}

	slog.Debug("table format fallback found devices", "component", "gpu", "count", len(devices))
	inv.Devices = devices
	inv.DeviceMemSnapshot = memSnapshot
	inv.enrichFromHostYAML()
	inv.LogInventory()
	return inv
}

// enrichFromHostYAML replaces truncated GPU names with correct names from the
// host YAML inventory, if available for the current hostname.
func (inv *GPUInventory) enrichFromHostYAML() {
	hostname, err := os.Hostname()
	if err != nil {
		return
	}
	if spec := inventory.FindHost(hostname); spec != nil {
		inv.EnrichNamesFromHostSpec(spec)
	}
}

func (inv *GPUInventory) detectInventoryHost() {
	hostname, err := os.Hostname()
	if err != nil {
		return
	}
	inv.isInventoryHost = inventory.FindHost(hostname) != nil
}

// RefreshDeviceMemSnapshot queries nvidia-smi for actual per-device memory usage
// and stores the result in DeviceMemSnapshot. Safe to call on non-GPU hosts
// (sets an empty map). Falls back to table format parsing on old drivers.
func (inv *GPUInventory) RefreshDeviceMemSnapshot() {
	result := make(map[string]DeviceMemInfo)
	utilByDevice := make(map[string]int)
	hasCompute := make(map[string]bool)
	rows := inv.queryNvidiaSmiCached("--query-gpu=index,memory.used,memory.total", 3)
	if len(rows) > 0 {
		for _, parts := range rows {
			idx := parts[0]
			used, _ := strconv.Atoi(parts[1])
			total, _ := strconv.Atoi(parts[2])
			result[idx] = DeviceMemInfo{UsedMiB: used, TotalMiB: total}
		}
	}
	utilRows := inv.queryNvidiaSmiCached("--query-gpu=index,utilization.gpu", 2)
	for _, parts := range utilRows {
		idx := parts[0]
		util, _ := strconv.Atoi(parts[1])
		utilByDevice[idx] = util
	}

	// Compute process detection on modern drivers: gpu_uuid -> index.
	computeRows := inv.queryNvidiaSmiCached("--query-compute-apps=gpu_uuid,pid,used_memory", 3)
	if len(computeRows) > 0 {
		uuidRows := inv.queryNvidiaSmiCached("--query-gpu=index,uuid", 2)
		uuidToIndex := make(map[string]string, len(uuidRows))
		for _, parts := range uuidRows {
			uuidToIndex[parts[1]] = parts[0]
		}
		for _, parts := range computeRows {
			uuid := parts[0]
			if idx, ok := uuidToIndex[uuid]; ok {
				hasCompute[idx] = true
			}
		}
	}

	// Fallback to table format for old drivers (or query-output incompatibility).
	if tableOut, err := execNvidiaSmiRaw(); err == nil {
		if _, memInfo := parseNvidiaSmiTable(tableOut); len(memInfo) > 0 {
			result = memInfo
		}
		if len(utilByDevice) == 0 {
			utilByDevice = parseNvidiaSmiTableUtil(tableOut)
		}
		if len(hasCompute) == 0 {
			hasCompute = parseNvidiaSmiTableComputeProcessFlags(tableOut)
		}
	}
	inv.DeviceMemSnapshot = result
	inv.DeviceUtilPct = utilByDevice
	inv.DeviceHasCompute = hasCompute
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
		slog.Debug("no GPU devices discovered", "component", "gpu")
		return
	}
	slog.Debug("GPU inventory discovered", "component", "gpu", "count", len(inv.Devices))
	for _, d := range inv.Devices {
		slog.Debug("GPU device", "component", "gpu", "index", d.Index, "name", d.Name, "mem_gb", d.TotalMemGB)
	}
}

// EnrichNamesFromHostSpec replaces truncated GPU names (containing "...")
// with correct names from the host YAML inventory. On old nvidia drivers
// (e.g. 525.x), nvidia-smi truncates names like "NVIDIA GeForce RTX 3090"
// to "NVIDIA GeForce ...", breaking class-based GPU matching.
func (inv *GPUInventory) EnrichNamesFromHostSpec(spec *inventory.HostSpec) {
	if spec == nil || len(spec.GPUs) == 0 {
		return
	}
	// Build index → name map from host spec
	nameByIndex := make(map[string]string)
	for _, g := range spec.GPUs {
		for _, idx := range g.Indices {
			nameByIndex[fmt.Sprintf("%d", idx)] = g.Name
		}
	}
	for i := range inv.Devices {
		if strings.Contains(inv.Devices[i].Name, "...") {
			if name, ok := nameByIndex[inv.Devices[i].Index]; ok {
				slog.Debug("GPU name enriched from host spec",
					"component", "gpu",
					"device", inv.Devices[i].Index,
					"truncated", inv.Devices[i].Name,
					"resolved", name)
				inv.Devices[i].Name = name
			}
		}
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
	for _, rs := range state.RunningSnapshot() {
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
	for _, rs := range state.RunningSnapshot() {
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

	// Check actual VRAM usage if snapshot is available.
	// Allow up to 512 MiB of driver/display overhead when the job requests
	// the full physical capacity (e.g., gpu_mem=24 on a 24GB GPU).
	const driverOverheadMiB = 512
	freeMiB = totalMem * 1024 // fallback: use static total in MiB
	if info, found := inv.DeviceMemSnapshot[device]; found {
		freeMiB = info.TotalMiB - info.UsedMiB
		memRequiredMiB := memRequiredGB * 1024
		if memRequiredMiB > freeMiB+driverOverheadMiB {
			return 0, false
		}
	}
	return freeMiB, true
}

// PickBestGPUForClass finds the best available GPU device for a given class.
// Returns the device index and true, or "" and false if none available.
func (inv *GPUInventory) PickBestGPUForClass(state *State, className string, memRequired int) (string, bool) {
	device, _, ok := inv.PickBestGPUForClassWithReason(state, className, memRequired)
	return device, ok
}

// PickBestGPUForClassWithReason finds the best available GPU device for a given class.
// Returns a user-facing reason when no device can currently satisfy the request.
func (inv *GPUInventory) PickBestGPUForClassWithReason(state *State, className string, memRequired int) (string, string, bool) {
	candidates := inv.DevicesByClass(className)
	if len(candidates) == 0 {
		slog.Debug("no matching GPU devices for class", "component", "gpu", "class", className, "total_devices", len(inv.Devices))
		return "", fmt.Sprintf("no GPU matching class %s", className), false
	}

	bestDevice, bestFreeMiB, busyCount, memBlockedCount := inv.pickBestDevice(state, candidates, memRequired)

	if bestDevice == "" {
		switch {
		case busyCount == len(candidates):
			return "", fmt.Sprintf("all %s GPUs are in use", className), false
		case memBlockedCount > 0:
			return "", fmt.Sprintf("no %s GPU currently has %dGB free", className, memRequired), false
		default:
			return "", fmt.Sprintf("no %s GPU is currently available", className), false
		}
	}
	slog.Debug("GPU device selected", "component", "gpu", "class", className, "device", bestDevice, "free_mib", bestFreeMiB)
	return bestDevice, "", true
}

// pickBestDevice selects the candidate device with the most free memory,
// skipping busy devices and those that fail the memory check.
func (inv *GPUInventory) pickBestDevice(state *State, candidates []string, memRequired int) (bestDevice string, bestFreeMiB int, busyCount int, memBlockedCount int) {
	bestFreeMiB = -1
	for _, device := range candidates {
		if inv.deviceBusy(state, device, false) {
			busyCount++
			continue
		}
		freeMiB, ok := inv.deviceMemCheck(state, device, memRequired)
		if !ok {
			memBlockedCount++
			continue
		}
		if freeMiB > bestFreeMiB {
			bestFreeMiB = freeMiB
			bestDevice = device
		}
	}
	return
}

// PickLeastLoadedGPU selects the GPU device with the most free memory,
// skipping devices that have running jobs. No class constraint or memory
// requirement is applied. Returns the device index, or "" if no device
// is available.
func (inv *GPUInventory) PickLeastLoadedGPU(state *State) string {
	candidates := make([]string, len(inv.Devices))
	for i, d := range inv.Devices {
		candidates[i] = d.Index
	}
	bestDevice, bestFreeMiB, _, _ := inv.pickBestDevice(state, candidates, 0)
	if bestDevice != "" {
		slog.Debug("GPU auto-assigned (no class constraint)", "component", "gpu", "device", bestDevice, "free_mib", bestFreeMiB)
	}
	return bestDevice
}

// CanStartGPUJob checks if a job can start based on GPU constraints.
// Returns true if the job can start, along with the resolved GPU devices.
// Checks both our own reservations and actual VRAM usage (from DeviceMemSnapshot).
func (inv *GPUInventory) CanStartGPUJob(state *State, job *RunnerJob) (bool, []string) {
	canStart, devices, _ := inv.CanStartGPUJobWithReason(state, job)
	return canStart, devices
}

// CanStartGPUJobWithReason checks if a job can start based on GPU constraints.
// Returns a user-facing reason when the GPU gate blocks the job.
func (inv *GPUInventory) CanStartGPUJobWithReason(state *State, job *RunnerJob) (bool, []string, string) {
	strictIsolation := HasTag(job.Data, "benchmark")

	// GPU class-based job
	if job.Data.GPUClass != "" {
		memPerDevice := GetJobGPUMem(job.Data, DefaultGPUMemGB)
		device, reason, ok := inv.pickBestGPUForClassWithMode(state, job.Data.GPUClass, memPerDevice, strictIsolation)
		if !ok {
			return false, nil, reason
		}
		return true, []string{device}, ""
	}

	devices := GetJobGPUDevices(job.Data)
	if len(devices) == 0 {
		// CPU-only job — always allowed
		return true, nil, ""
	}

	memPerDevice := GetJobGPUMem(job.Data, DefaultGPUMemGB)
	for _, device := range devices {
		if inv.deviceBusy(state, device, strictIsolation) {
			return false, nil, fmt.Sprintf("GPU %s is already in use", device)
		}
		if _, ok := inv.deviceMemCheck(state, device, memPerDevice); !ok {
			return false, nil, fmt.Sprintf("GPU %s does not currently have %dGB free", device, memPerDevice)
		}
	}
	return true, devices, ""
}

const defaultNonBenchmarkGPUBusyUtilThreshold = 50

func (inv *GPUInventory) deviceBusy(state *State, device string, strictIsolation bool) bool {
	if DeviceHasRunningJob(state, device) {
		return true
	}
	// Apply compute-process occupancy gating only on inventory (on-prem) hosts.
	if !inv.isInventoryHost {
		return false
	}
	if !inv.DeviceHasCompute[device] {
		return false
	}
	if strictIsolation {
		return true
	}
	util := inv.DeviceUtilPct[device]
	return util >= defaultNonBenchmarkGPUBusyUtilThreshold
}

func (inv *GPUInventory) pickBestGPUForClassWithMode(state *State, className string, memRequired int, strictIsolation bool) (string, string, bool) {
	candidates := inv.DevicesByClass(className)
	if len(candidates) == 0 {
		slog.Debug("no matching GPU devices for class", "component", "gpu", "class", className, "total_devices", len(inv.Devices))
		return "", fmt.Sprintf("no GPU matching class %s", className), false
	}

	bestDevice := ""
	bestFreeMiB := -1
	busyCount := 0
	memBlockedCount := 0
	for _, device := range candidates {
		if inv.deviceBusy(state, device, strictIsolation) {
			busyCount++
			continue
		}
		freeMiB, ok := inv.deviceMemCheck(state, device, memRequired)
		if !ok {
			memBlockedCount++
			continue
		}
		if freeMiB > bestFreeMiB {
			bestFreeMiB = freeMiB
			bestDevice = device
		}
	}
	if bestDevice == "" {
		switch {
		case busyCount == len(candidates):
			return "", fmt.Sprintf("all %s GPUs are in use", className), false
		case memBlockedCount > 0:
			return "", fmt.Sprintf("no %s GPU currently has %dGB free", className, memRequired), false
		default:
			return "", fmt.Sprintf("no %s GPU is currently available", className), false
		}
	}
	slog.Debug("GPU device selected", "component", "gpu", "class", className, "device", bestDevice, "free_mib", bestFreeMiB)
	return bestDevice, "", true
}

// DefaultGPUMemGB is the default GPU memory reservation when a job uses a GPU.
const DefaultGPUMemGB = 20

// RunnerJob wraps a CommandJob with runtime metadata.
type RunnerJob struct {
	Data *opsqueue.CommandJob
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
