package hostinfo

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HostStatus represents the connectivity status of a host
type HostStatus int

const (
	HostStatusUnknown HostStatus = iota
	HostStatusChecking
	HostStatusOnline
	HostStatusOffline
)

// QueueCheckStatus represents the status of queue info fetching
type QueueCheckStatus int

const (
	QueueCheckUnknown QueueCheckStatus = iota
	QueueCheckChecking
	QueueCheckChecked
)

// GPUInfo contains information about a single GPU
type GPUInfo struct {
	Index       int
	Name        string
	Temperature int    // Celsius
	Utilization int    // Percentage
	MemUsed     string // e.g., "12 MiB"
	MemTotal    string // e.g., "80 GiB"
	JobID       int64  // Job using this GPU (0 if unknown/none)
	JobLabel    string // Short label for job (e.g., "#42 train.py")
}

// JobGPUUsage tracks GPU usage for a single job
type JobGPUUsage struct {
	GPUIndex int
	MemUsed  string // e.g., "12345" (MiB)
}

// HostRunningJob represents a job running on a host
type HostRunningJob struct {
	ID          int64
	Description string
	Command     string
	DeclaredGPU string        // GPU from CUDA_VISIBLE_DEVICES in command
	GPUs        []JobGPUUsage // GPUs this job is using (from nvidia-smi)
}

// Host represents a remote host with its system information
type Host struct {
	Name      string
	Status    HostStatus
	Arch      string // e.g., "Linux x86_64", "Darwin arm64"
	OS        string // e.g., "5.15.0-generic"
	Model     string // e.g., "Mac14,6" or "MacBook Pro (16-inch, 2023)"
	CPUs      int
	CPUModel  string // e.g., "Apple M2 Max" or "Intel Core i9-9900K"
	CPUFreq   string // e.g., "3.2 GHz"
	MemTotal  string // e.g., "128G"
	MemUsed   string // e.g., "58G"
	LoadAvg   string // e.g., "0.5, 0.3, 0.2"
	DiskFree  int64  // Free disk space in bytes (for home directory)
	DiskTotal int64  // Total disk space in bytes (for home directory)
	GPUs      []GPUInfo
	LastCheck time.Time
	Error     string // connection error message (not displayed as error)

	// Queue status
	QueueStatus       QueueCheckStatus // Unknown, Checking, Checked
	QueueRunnerActive bool             // Whether queue runner tmux session exists
	QueuedJobCount    int              // Number of jobs waiting in queue
	CurrentQueueJob   string           // Job ID currently running in queue
	QueueStopPending  bool             // Whether stop signal file exists
	BlockedQueueJobs  map[int64]string // Queued jobs held by a gate, with reason
	SyncWarning       string           // Latest session-only sync warning shown in the TUI

	// Running jobs on this host
	RunningJobs []HostRunningJob

	lastCPUPct int
	lastRAMPct int
	lastGPUPct int
}

// HostInfoCommand is the SSH command to gather host information
// It outputs structured lines that parseHostInfo can parse
// GPU info is parsed from standard nvidia-smi output for maximum compatibility
// The awk script captures GPU name lines and their following stats lines
const HostInfoCommand = `echo "ARCH:$(uname -sm)"; ` +
	`echo "OS:$(uname -r)"; ` +
	`echo "CPUS:$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo -)"; ` +
	`echo "LOAD:$(uptime | sed 's/.*load average[s]*: //')"; ` +
	// Disk space: df -k on home directory, output in 1K blocks (portable)
	`df -k ~ 2>/dev/null | awk 'NR==2 {print "DISK:" $2 ":" $4}' || true; ` +
	// Memory: Linux uses free, macOS uses sysctl + vm_stat
	`if command -v free >/dev/null 2>&1; then ` +
	`echo "MEM:$(free -h | awk '/^Mem:/ {print $2":"$3}')"; ` +
	`else ` +
	// macOS: get total from sysctl, used from vm_stat (active + wired + compressed)
	`total_gb=$(sysctl -n hw.memsize 2>/dev/null | awk '{printf "%.0f", $1/1024/1024/1024}'); ` +
	`page_size=$(sysctl -n hw.pagesize 2>/dev/null); ` +
	`vm_out=$(vm_stat 2>/dev/null); ` +
	`pages_active=$(echo "$vm_out" | awk '/Pages active/ {gsub(/\./,"",$3); print $3}'); ` +
	`pages_wired=$(echo "$vm_out" | awk '/Pages wired/ {gsub(/\./,"",$4); print $4}'); ` +
	`pages_comp=$(echo "$vm_out" | awk '/Pages occupied by compressor/ {gsub(/\./,"",$5); print $5}'); ` +
	`if [ -n "$pages_active" ] && [ -n "$page_size" ]; then ` +
	`used_gb=$(( (pages_active + pages_wired + pages_comp) * page_size / 1024 / 1024 / 1024 )); ` +
	`echo "MEM:${total_gb}G:${used_gb}G"; ` +
	`else ` +
	`echo "MEM:${total_gb}G:-"; ` +
	`fi; ` +
	`fi; ` +
	// Model: macOS hw.model
	`sysctl -n hw.model 2>/dev/null | sed 's/^/MODEL:/' || true; ` +
	// CPU model: macOS uses brand_string, Linux uses /proc/cpuinfo
	`(sysctl -n machdep.cpu.brand_string 2>/dev/null || grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2) | sed 's/^[[:space:]]*//' | sed 's/^/CPUMODEL:/' || true; ` +
	// macOS GPU: system_profiler (brief format)
	`system_profiler SPDisplaysDataType 2>/dev/null | grep -E '(Chipset Model|VRAM|Total Number of Cores|Metal)' | sed 's/^[[:space:]]*/MACGPU:/' || true; ` +
	// Linux GPU: nvidia-smi table output (name + stats lines)
	`nvidia-smi 2>/dev/null | awk '/^\|[[:space:]]+[0-9]+[[:space:]]+[A-Z]/ { print "GPUNAME:" $0; getline; print "GPUSTAT:" $0 }'; ` +
	// Linux GPU: nvidia-smi -L for untruncated names (old drivers truncate table output)
	`nvidia-smi -L 2>/dev/null | sed 's/^/GPULIST:/'`

// ParseHostInfo parses the output of HostInfoCommand into a Host struct
func ParseHostInfo(output string) *Host {
	host := &Host{
		Status:    HostStatusOnline,
		LastCheck: time.Now(),
	}

	// Track pending GPU info (name parsed, waiting for stats)
	var pendingGPU *GPUInfo
	// Untruncated GPU names from nvidia-smi -L, keyed by index
	gpuListNames := map[int]string{}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if idx := strings.Index(line, ":"); idx > 0 {
			key := line[:idx]
			value := strings.TrimSpace(line[idx+1:])

			switch key {
			case "ARCH":
				host.Arch = value
			case "OS":
				host.OS = value
			case "MODEL":
				host.Model = value
			case "CPUMODEL":
				host.CPUModel = value
			case "CPUS":
				if n, err := strconv.Atoi(value); err == nil {
					host.CPUs = n
				}
			case "LOAD":
				host.LoadAvg = strings.TrimSpace(value)
			case "MEM":
				parts := strings.SplitN(value, ":", 2)
				if len(parts) == 2 {
					host.MemTotal = parts[0]
					host.MemUsed = parts[1]
					// Clean up "-" for unused values (macOS doesn't report used)
					if host.MemUsed == "-" {
						host.MemUsed = ""
					}
				}
			case "DISK":
				// Format: total_kb:avail_kb (from df -k)
				parts := strings.SplitN(value, ":", 2)
				if len(parts) == 2 {
					if total, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
						host.DiskTotal = total * 1024 // Convert KB to bytes
					}
					if free, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						host.DiskFree = free * 1024 // Convert KB to bytes
					}
				}
			case "MACGPU":
				// Parse macOS GPU info lines
				parseMacGPULine(value, host)
			case "GPU":
				gpu := parseGPULine(value)
				if gpu != nil {
					host.GPUs = append(host.GPUs, *gpu)
				}
			case "GPUNAME":
				// Save pending GPU, parse name line
				if pendingGPU != nil {
					host.GPUs = append(host.GPUs, *pendingGPU)
				}
				pendingGPU = parseNvidiaSmiNameLine(value)
			case "GPUSTAT":
				// Parse stats and merge with pending GPU
				if pendingGPU != nil {
					parseNvidiaSmiStatsLine(value, pendingGPU)
					host.GPUs = append(host.GPUs, *pendingGPU)
					pendingGPU = nil
				}
			case "GPULINE":
				// Legacy: single line format (name only)
				gpu := parseNvidiaSmiNameLine(value)
				if gpu != nil {
					host.GPUs = append(host.GPUs, *gpu)
				}
			case "GPULIST":
				// nvidia-smi -L output: "GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-...)"
				if gpuIdx, name := parseGPUListLine(value); name != "" {
					gpuListNames[gpuIdx] = name
				}
			}
		}
	}

	// Don't forget any pending GPU
	if pendingGPU != nil {
		host.GPUs = append(host.GPUs, *pendingGPU)
	}

	// Backfill truncated GPU names from nvidia-smi -L output.
	// Old drivers (e.g. 525.x) truncate names in the table to "NVIDIA GeForce ..."
	// but -L always outputs the full name.
	if len(gpuListNames) > 0 {
		for i := range host.GPUs {
			if fullName, ok := gpuListNames[host.GPUs[i].Index]; ok {
				if len(fullName) > len(host.GPUs[i].Name) {
					host.GPUs[i].Name = fullName
				}
			}
		}
	}

	return host
}

// parseNvidiaSmiNameLine parses the GPU name line from standard nvidia-smi output
// Format: |   0  NVIDIA GeForce ...  On   | 00000000:01:00.0 Off |                  N/A |
func parseNvidiaSmiNameLine(line string) *GPUInfo {
	// Remove leading/trailing pipe characters and whitespace
	line = strings.Trim(line, "| ")
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return nil
	}

	// First field is GPU index
	idx, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil
	}

	// Second field should be a GPU vendor name (NVIDIA, AMD, etc.), not "N/A"
	// Skip process lines which have "N/A" as the second field
	if fields[1] == "N/A" {
		return nil
	}

	gpu := &GPUInfo{Index: idx}

	// Build GPU name from remaining fields until we hit a non-name field
	var nameParts []string
	for i := 1; i < len(fields); i++ {
		// Stop at common end markers
		if fields[i] == "On" || fields[i] == "Off" || strings.HasPrefix(fields[i], "0000") {
			break
		}
		nameParts = append(nameParts, fields[i])
	}
	gpu.Name = strings.Join(nameParts, " ")
	// Remove trailing "..." from truncated names
	gpu.Name = strings.TrimSuffix(gpu.Name, "...")

	return gpu
}

// parseGPUListLine parses a line from nvidia-smi -L output.
// Format: "GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-abcdef12-...)"
// Returns the GPU index and untruncated name, or (-1, "") on parse failure.
func parseGPUListLine(line string) (int, string) {
	// Expect "GPU <idx>: <name> (UUID: ...)"
	if !strings.HasPrefix(line, "GPU ") {
		return -1, ""
	}
	rest := line[4:] // after "GPU "
	colonIdx := strings.Index(rest, ":")
	if colonIdx < 0 {
		return -1, ""
	}
	idx, err := strconv.Atoi(strings.TrimSpace(rest[:colonIdx]))
	if err != nil {
		return -1, ""
	}
	nameAndUUID := strings.TrimSpace(rest[colonIdx+1:])
	// Strip "(UUID: ...)" suffix
	if parenIdx := strings.Index(nameAndUUID, " (UUID:"); parenIdx > 0 {
		nameAndUUID = nameAndUUID[:parenIdx]
	}
	return idx, strings.TrimSpace(nameAndUUID)
}

// parseNvidiaSmiStatsLine parses the GPU stats line from standard nvidia-smi output
// Format: | 30%   45C    P8    20W / 350W |    123MiB / 24564MiB |      0%      Default |
func parseNvidiaSmiStatsLine(line string, gpu *GPUInfo) {
	if gpu == nil {
		return
	}

	// Split by | to get the three sections
	sections := strings.Split(line, "|")
	if len(sections) < 4 {
		return
	}

	// Section 1: Fan%, Temp, Perf, Power
	// e.g., " 30%   45C    P8    20W / 350W "
	section1 := strings.Fields(sections[1])
	for i, field := range section1 {
		// Temperature ends with C
		if strings.HasSuffix(field, "C") {
			tempStr := strings.TrimSuffix(field, "C")
			if temp, err := strconv.Atoi(tempStr); err == nil {
				gpu.Temperature = temp
			}
		}
		// Fan percentage (first field ending with %)
		if i == 0 && strings.HasSuffix(field, "%") {
			// This is fan speed, not GPU utilization - skip
		}
	}

	// Section 2: Memory usage
	// e.g., "    123MiB / 24564MiB "
	section2 := strings.TrimSpace(sections[2])
	memParts := strings.Split(section2, "/")
	if len(memParts) == 2 {
		gpu.MemUsed = strings.TrimSpace(memParts[0])
		gpu.MemTotal = strings.TrimSpace(memParts[1])
	}

	// Section 3: GPU utilization
	// e.g., "      0%      Default "
	section3 := strings.Fields(sections[3])
	for _, field := range section3 {
		if strings.HasSuffix(field, "%") {
			utilStr := strings.TrimSuffix(field, "%")
			if util, err := strconv.Atoi(utilStr); err == nil {
				gpu.Utilization = util
				break
			}
		}
	}
}

// parseMacGPULine parses macOS system_profiler GPU info lines
// Lines look like: "Chipset Model: Apple M2 Max" or "VRAM (Total): 38 GB"
func parseMacGPULine(line string, host *Host) {
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "Chipset Model:") {
		name := strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		// Create new GPU entry
		gpu := GPUInfo{
			Index: len(host.GPUs),
			Name:  name,
		}
		host.GPUs = append(host.GPUs, gpu)
	} else if strings.HasPrefix(line, "VRAM") && len(host.GPUs) > 0 {
		// VRAM (Total): 38 GB or VRAM (Dynamic, Max): 48 GB
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			host.GPUs[len(host.GPUs)-1].MemTotal = strings.TrimSpace(parts[1])
		}
	} else if strings.HasPrefix(line, "Total Number of Cores:") && len(host.GPUs) > 0 {
		cores := strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:"))
		// Append core count to GPU name
		host.GPUs[len(host.GPUs)-1].Name += " (" + cores + " cores)"
	}
}

// parseGPULine parses a single nvidia-smi CSV output line
// Format: index, name, temperature, utilization, memory.used, memory.total
func parseGPULine(line string) *GPUInfo {
	parts := strings.Split(line, ",")
	if len(parts) < 6 {
		return nil
	}

	// Trim spaces from all parts
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	gpu := &GPUInfo{
		Name:     parts[1],
		MemUsed:  parts[4] + " MiB",
		MemTotal: parts[5] + " MiB",
	}

	if idx, err := strconv.Atoi(parts[0]); err == nil {
		gpu.Index = idx
	}
	if temp, err := strconv.Atoi(parts[2]); err == nil {
		gpu.Temperature = temp
	}
	if util, err := strconv.Atoi(parts[3]); err == nil {
		gpu.Utilization = util
	}

	return gpu
}

// UpdateFrom merges non-zero fields from source into h.
// Zero-valued fields in source are skipped, preserving existing data in h.
// The Name field is never overwritten (identity field).
// Status is always overwritten from source.
func (h *Host) UpdateFrom(source *Host) {
	if h == nil || source == nil {
		return
	}
	// Status: always overwrite
	h.Status = source.Status

	if source.Arch != "" {
		h.Arch = source.Arch
	}
	if source.OS != "" {
		h.OS = source.OS
	}
	if source.Model != "" {
		h.Model = source.Model
	}
	if source.CPUs != 0 {
		h.CPUs = source.CPUs
	}
	if source.CPUModel != "" {
		h.CPUModel = source.CPUModel
	}
	if source.CPUFreq != "" {
		h.CPUFreq = source.CPUFreq
	}
	if source.MemTotal != "" {
		h.MemTotal = source.MemTotal
	}
	if source.MemUsed != "" {
		h.MemUsed = source.MemUsed
	}
	if source.LoadAvg != "" {
		h.LoadAvg = source.LoadAvg
	}
	if source.DiskFree != 0 {
		h.DiskFree = source.DiskFree
	}
	if source.DiskTotal != 0 {
		h.DiskTotal = source.DiskTotal
	}
	if source.GPUs != nil {
		h.GPUs = source.GPUs
	}
	if !source.LastCheck.IsZero() {
		h.LastCheck = source.LastCheck
	}
	if source.Error != "" {
		h.Error = source.Error
	} else if source.Status == HostStatusOnline {
		h.Error = ""
	}

	// Queue fields
	if source.QueueStatus != QueueCheckUnknown {
		h.QueueStatus = source.QueueStatus
		h.QueueRunnerActive = source.QueueRunnerActive
		h.QueuedJobCount = source.QueuedJobCount
		h.CurrentQueueJob = source.CurrentQueueJob
		h.QueueStopPending = source.QueueStopPending
	}
	if source.BlockedQueueJobs != nil {
		if len(source.BlockedQueueJobs) == 0 {
			h.BlockedQueueJobs = nil
		} else {
			h.BlockedQueueJobs = make(map[int64]string, len(source.BlockedQueueJobs))
			for jobID, reason := range source.BlockedQueueJobs {
				h.BlockedQueueJobs[jobID] = reason
			}
		}
	}
	if source.RunningJobs != nil {
		h.RunningJobs = source.RunningJobs
	}
}

// StatusString returns a human-readable status string
func (h *Host) StatusString() string {
	switch h.Status {
	case HostStatusOnline:
		return "online"
	case HostStatusOffline:
		return "offline"
	case HostStatusChecking:
		return "checking"
	default:
		return "unknown"
	}
}

// GPUSummary returns a brief GPU summary for the list view
func (h *Host) GPUSummary() string {
	if len(h.GPUs) == 0 {
		return "-"
	}

	// Extract short GPU name (e.g., "A100" from "NVIDIA A100-SXM4-80GB")
	name := h.GPUs[0].Name
	name = strings.TrimPrefix(name, "NVIDIA ")
	if idx := strings.Index(name, "-"); idx > 0 {
		name = name[:idx]
	}

	// Calculate average utilization across all GPUs
	var totalUtil int
	var hasUtil bool
	for _, gpu := range h.GPUs {
		if gpu.Utilization > 0 || gpu.MemUsed != "" {
			totalUtil += gpu.Utilization
			hasUtil = true
		}
	}

	var summary string
	if len(h.GPUs) == 1 {
		summary = name
	} else {
		summary = strconv.Itoa(len(h.GPUs)) + "×" + name
	}

	if hasUtil {
		avgUtil := totalUtil / len(h.GPUs)
		summary += fmt.Sprintf(" %d%%", avgUtil)
	}

	return summary
}

// LoadAvgShort returns just the 1-minute load average
func (h *Host) LoadAvgShort() string {
	if h.LoadAvg == "" {
		return "-"
	}
	parts := strings.SplitN(h.LoadAvg, ",", 2)
	return strings.TrimSpace(parts[0])
}

// CPUUtilizationPct returns CPU utilization as a percentage int based on 1-minute load average
// Returns -1 if unavailable
func (h *Host) CPUUtilizationPct() int {
	if h.LoadAvg == "" || h.CPUs == 0 {
		return -1
	}
	// Parse 1-minute load average
	loadStr := strings.ReplaceAll(h.LoadAvg, ",", " ")
	loads := strings.Fields(loadStr)
	if len(loads) == 0 {
		return -1
	}
	loadVal, err := strconv.ParseFloat(loads[0], 64)
	if err != nil {
		return -1
	}
	return int((loadVal / float64(h.CPUs)) * 100)
}

// CPUUtilization returns CPU utilization as a percentage string based on 1-minute load average
func (h *Host) CPUUtilization() string {
	pct := h.CPUUtilizationPct()
	if pct < 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", pct)
}

// parseMemGB parses memory values (handles formats like "128G", "58G", "128Gi", "58Gi", "128GiB")
func parseMemGB(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "iB")
	s = strings.TrimSuffix(s, "i")
	s = strings.TrimSuffix(s, "B")
	multiplier := 1.0
	if strings.HasSuffix(s, "G") {
		s = strings.TrimSuffix(s, "G")
		multiplier = 1.0
	} else if strings.HasSuffix(s, "M") {
		s = strings.TrimSuffix(s, "M")
		multiplier = 1.0 / 1024.0
	} else if strings.HasSuffix(s, "T") {
		s = strings.TrimSuffix(s, "T")
		multiplier = 1024.0
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return val * multiplier
}

// RAMUtilizationPct returns RAM utilization as a percentage int
// Returns -1 if unavailable
func (h *Host) RAMUtilizationPct() int {
	if h.MemTotal == "" || h.MemUsed == "" {
		return -1
	}
	total := parseMemGB(h.MemTotal)
	used := parseMemGB(h.MemUsed)
	if total == 0 {
		return -1
	}
	return int((used / total) * 100)
}

// RAMUtilization returns RAM utilization as a percentage string
func (h *Host) RAMUtilization() string {
	pct := h.RAMUtilizationPct()
	if pct < 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", pct)
}

// QueueSummary returns a brief queue status string for the list view
func (h *Host) QueueSummary() string {
	switch h.QueueStatus {
	case QueueCheckUnknown, QueueCheckChecking:
		return "-"
	case QueueCheckChecked:
		if !h.QueueRunnerActive {
			return "○"
		}
		if h.QueueStopPending {
			return fmt.Sprintf("■ %d", h.QueuedJobCount)
		}
		return fmt.Sprintf("▶ %d", h.QueuedJobCount)
	default:
		return "-"
	}
}

// LowDiskThreshold is the threshold below which disk space is considered critically low (5MB)
const LowDiskThreshold = 5 * 1024 * 1024 // 5 MB in bytes

// HasLowDiskSpace returns true if the host has less than 5MB free disk space
func (h *Host) HasLowDiskSpace() bool {
	// Only warn if we have disk info and the host is online
	if h.DiskFree == 0 || h.Status != HostStatusOnline {
		return false
	}
	return h.DiskFree < LowDiskThreshold
}

// DiskFreeSummary returns a human-readable disk free space string
func (h *Host) DiskFreeSummary() string {
	if h.DiskFree == 0 {
		return ""
	}
	return formatBytes(h.DiskFree)
}

// DiskSummary returns a summary of disk usage (e.g., "45G free" or "4.2M free")
func (h *Host) DiskSummary() string {
	if h.DiskFree == 0 {
		return "-"
	}
	return formatBytes(h.DiskFree) + " free"
}

// formatBytes formats bytes as human-readable string
func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1fT", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1fG", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1fM", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1fK", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
