package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
)

// apiHost is the JSON representation of a host for the API.
type apiHost struct {
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	CPUCores  int      `json:"cpu_cores"`
	Memory    string   `json:"memory"`
	NetworkBW string   `json:"network_bw"`
	GPUs      []apiGPU `json:"gpus"`
	Status    string   `json:"status"`
	CPULoad   string   `json:"cpu_load,omitempty"`
	MemUsage  string   `json:"mem_usage,omitempty"`
	GPULoad   string   `json:"gpu_load,omitempty"`
	QueueInfo string   `json:"queue_info,omitempty"`
}

type apiGPU struct {
	Name        string `json:"name"`
	Class       string `json:"class"`
	Memory      string `json:"memory"`
	Count       int    `json:"count"`
	Utilization int    `json:"utilization"` // 0-100, live from monitor
	MemUsed     string `json:"mem_used,omitempty"`
	MemTotal    string `json:"mem_total,omitempty"`
	Temperature int    `json:"temperature,omitempty"`
}

// apiCoordinatorState is the JSON representation of the coordinator state.
type apiCoordinatorState struct {
	Running    bool   `json:"running"`
	QueueDepth int    `json:"queue_depth,omitempty"`
	PID        string `json:"pid,omitempty"`
}

func (s *Server) handleAPIHosts(w http.ResponseWriter, _ *http.Request) {
	specs, err := inventory.LoadHosts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get live host data from monitor
	var liveHosts []*hostinfo.Host
	if s.monitor != nil {
		liveHosts = s.monitor.Hosts()
	}
	liveMap := make(map[string]*hostinfo.Host)
	for _, h := range liveHosts {
		if h != nil {
			liveMap[h.Name] = h
		}
	}

	result := make([]apiHost, 0, len(specs))
	for _, spec := range specs {
		h := apiHost{
			Name:      spec.Name,
			OS:        spec.OS,
			Arch:      spec.Arch,
			CPUCores:  spec.CPUCores,
			Memory:    spec.Memory,
			NetworkBW: spec.NetworkBW,
			Status:    "unknown",
		}

		// Start with static GPU specs
		for _, gpu := range spec.GPUs {
			h.GPUs = append(h.GPUs, apiGPU{
				Name:   gpu.Name,
				Class:  gpu.Class,
				Memory: gpu.Memory,
				Count:  len(gpu.Indices),
			})
		}

		// Merge live data from monitor
		if live, ok := liveMap[spec.Name]; ok {
			h.Status = statusLabel(live.Status)
			if pct, ok := hostinfo.HostCPULoadPercent(live); ok {
				h.CPULoad = fmt.Sprintf("%d%%", pct)
			}
			if pct, ok := hostinfo.HostMemUsagePercent(live); ok {
				h.MemUsage = fmt.Sprintf("%d%%", pct)
			}
			if pct, ok := hostinfo.HostGPULoadPercent(live); ok {
				h.GPULoad = fmt.Sprintf("%d%%", pct)
			}
			h.QueueInfo = live.QueueSummary()

			// Merge per-GPU live utilization into static GPU list
			mergeLiveGPUs(h.GPUs, live.GPUs)
		}

		result = append(result, h)
	}

	writeJSON(w, result)
}

// mergeLiveGPUs overlays live GPU metrics onto the static API GPU list.
// It matches by expanding static GPU groups (which have Count>1) to live GPUs by index.
func mergeLiveGPUs(apiGPUs []apiGPU, liveGPUs []hostinfo.GPUInfo) {
	if len(liveGPUs) == 0 {
		return
	}

	// Build index->liveGPU map
	liveByIndex := make(map[int]*hostinfo.GPUInfo, len(liveGPUs))
	for i := range liveGPUs {
		liveByIndex[liveGPUs[i].Index] = &liveGPUs[i]
	}

	// For each API GPU group, aggregate live metrics from matching indices.
	// The static GPU list uses Count to represent a group (e.g., 2x A100).
	// We map to live GPUs using the inventory's index ranges.
	specs, _ := inventory.LoadHosts()
	specIndices := make(map[string]map[string][]int) // host->class->indices (not available here)
	_ = specIndices
	_ = specs

	// Simple approach: match live GPUs to API GPU groups by name similarity
	// Each API GPU group may represent multiple physical GPUs (Count field).
	// Aggregate: pick max utilization and sum memory across the group.
	idx := 0
	for i := range apiGPUs {
		count := apiGPUs[i].Count
		if count <= 0 {
			count = 1
		}
		var maxUtil, maxTemp int
		var totalMemUsed, totalMemTotal string
		matched := 0
		for j := 0; j < count; j++ {
			if live, ok := liveByIndex[idx]; ok {
				if live.Utilization > maxUtil {
					maxUtil = live.Utilization
				}
				if live.Temperature > maxTemp {
					maxTemp = live.Temperature
				}
				// Use last GPU's memory values (they're the same within a group)
				if live.MemUsed != "" {
					totalMemUsed = live.MemUsed
				}
				if live.MemTotal != "" {
					totalMemTotal = live.MemTotal
				}
				matched++
			}
			idx++
		}
		if matched > 0 {
			apiGPUs[i].Utilization = maxUtil
			apiGPUs[i].Temperature = maxTemp
			apiGPUs[i].MemUsed = totalMemUsed
			apiGPUs[i].MemTotal = totalMemTotal
		}
	}
}

func (s *Server) handleAPICoordinator(w http.ResponseWriter, _ *http.Request) {
	state := apiCoordinatorState{}

	// Check if coordinator PID file exists
	home, _ := os.UserHomeDir()
	pidFile := filepath.Join(home, ".cache", "weft", "coordinator.pid")
	data, err := os.ReadFile(pidFile)
	if err == nil {
		state.PID = string(data)
		state.Running = true
	}

	writeJSON(w, state)
}

func (s *Server) handleAPIOplog(w http.ResponseWriter, _ *http.Request) {
	entries, err := oplog.ReadRecent(50)
	if err != nil {
		// Return empty array rather than error — log may not exist yet
		writeJSON(w, []oplog.Entry{})
		return
	}

	// Reverse to show newest first
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	writeJSON(w, entries)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
