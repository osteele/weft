package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

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
}

type apiGPU struct {
	Name   string `json:"name"`
	Class  string `json:"class"`
	Memory string `json:"memory"`
	Count  int    `json:"count"`
}

// apiCoordinatorState is the JSON representation of the coordinator state.
type apiCoordinatorState struct {
	Running    bool   `json:"running"`
	QueueDepth int    `json:"queue_depth,omitempty"`
	PID        string `json:"pid,omitempty"`
}

func (s *Server) handleAPIHosts(w http.ResponseWriter, r *http.Request) {
	specs, err := inventory.LoadEmbeddedHosts()
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
		}
		for _, gpu := range spec.GPUs {
			h.GPUs = append(h.GPUs, apiGPU{
				Name:   gpu.Name,
				Class:  gpu.Class,
				Memory: gpu.Memory,
				Count:  len(gpu.Indices),
			})
		}

		if live, ok := liveMap[spec.Name]; ok {
			h.Status = statusLabel(live.Status)
			if pct, ok := hostinfo.HostCPULoadPercent(live); ok {
				h.CPULoad = strconv.Itoa(pct) + "%"
			}
			if pct, ok := hostinfo.HostMemUsagePercent(live); ok {
				h.MemUsage = strconv.Itoa(pct) + "%"
			}
		} else {
			h.Status = "unknown"
		}

		result = append(result, h)
	}

	writeJSON(w, result)
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
