package tui

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

// GPUInfo contains information about a single GPU
type GPUInfo struct {
	Index       int
	Name        string
	Temperature int    // Celsius
	Utilization int    // Percentage
	MemUsed     string // e.g., "12 MiB"
	MemTotal    string // e.g., "80 GiB"
}

// Host represents a remote host with its system information
type Host struct {
	Name      string
	Status    HostStatus
	Arch      string // e.g., "Linux x86_64", "Darwin arm64"
	OS        string // e.g., "5.15.0-generic"
	CPUs      int
	MemTotal  string // e.g., "128G"
	MemUsed   string // e.g., "58G"
	LoadAvg   string // e.g., "0.5, 0.3, 0.2"
	GPUs      []GPUInfo
	LastCheck time.Time
	Error     string // connection error message (not displayed as error)
}

// HostInfoCommand is the SSH command to gather host information
// It outputs structured lines that parseHostInfo can parse
// GPU info is parsed from standard nvidia-smi output for maximum compatibility
// The awk script captures GPU name lines and their following stats lines
const HostInfoCommand = `echo "ARCH:$(uname -sm)"; ` +
	`echo "OS:$(uname -r)"; ` +
	`echo "CPUS:$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo -)"; ` +
	`echo "LOAD:$(uptime | sed 's/.*load average[s]*: //')"; ` +
	`echo "MEM:$(free -h 2>/dev/null | awk '/^Mem:/ {print $2":"$3}' || echo -)"; ` +
	`nvidia-smi 2>/dev/null | awk '/^\|[[:space:]]+[0-9]+[[:space:]]+[A-Z]/ { print "GPUNAME:" $0; getline; print "GPUSTAT:" $0 }'`

// ParseHostInfo parses the output of HostInfoCommand into a Host struct
func ParseHostInfo(output string) *Host {
	host := &Host{
		Status:    HostStatusOnline,
		LastCheck: time.Now(),
	}

	// Track pending GPU info (name parsed, waiting for stats)
	var pendingGPU *GPUInfo

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
				}
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
			}
		}
	}

	// Don't forget any pending GPU
	if pendingGPU != nil {
		host.GPUs = append(host.GPUs, *pendingGPU)
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
