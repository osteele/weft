package hostinfo

import (
	"encoding/json"
	"time"

	"github.com/osteele/weft/internal/db"
)

// HostFromCachedInfo creates a Host from cached DB info.
func HostFromCachedInfo(cached *db.CachedHostInfo) *Host {
	if cached == nil {
		return nil
	}
	host := &Host{
		Name:      cached.Name,
		Status:    HostStatusUnknown, // Will be updated when we query
		Arch:      cached.Arch,
		OS:        cached.OSVersion,
		Model:     cached.Model,
		CPUs:      cached.CPUCount,
		CPUModel:  cached.CPUModel,
		CPUFreq:   cached.CPUFreq,
		MemTotal:  cached.MemTotal,
		LastCheck: time.Unix(cached.LastUpdated, 0),
	}

	if cached.GPUsJSON != "" {
		var gpus []GPUInfo
		if err := json.Unmarshal([]byte(cached.GPUsJSON), &gpus); err == nil {
			host.GPUs = gpus
		}
	}

	return host
}

// CachedInfoFromHost creates cached host info from a Host.
func CachedInfoFromHost(host *Host) *db.CachedHostInfo {
	if host == nil {
		return nil
	}
	cached := &db.CachedHostInfo{
		Name:        host.Name,
		Arch:        host.Arch,
		OSVersion:   host.OS,
		Model:       host.Model,
		CPUCount:    host.CPUs,
		CPUModel:    host.CPUModel,
		CPUFreq:     host.CPUFreq,
		MemTotal:    host.MemTotal,
		LastUpdated: time.Now().Unix(),
	}

	if len(host.GPUs) > 0 {
		if data, err := json.Marshal(host.GPUs); err == nil {
			cached.GPUsJSON = string(data)
		}
	}

	return cached
}

// UpdateHostWithCachedStatic updates a host's static fields from cached data.
func UpdateHostWithCachedStatic(host *Host, cached *Host) {
	if host == nil || cached == nil {
		return
	}
	if host.Arch == "" {
		host.Arch = cached.Arch
	}
	if host.OS == "" {
		host.OS = cached.OS
	}
	if host.Model == "" {
		host.Model = cached.Model
	}
	if host.CPUs == 0 {
		host.CPUs = cached.CPUs
	}
	if host.CPUModel == "" {
		host.CPUModel = cached.CPUModel
	}
	if host.CPUFreq == "" {
		host.CPUFreq = cached.CPUFreq
	}
	if host.MemTotal == "" {
		host.MemTotal = cached.MemTotal
	}
	if len(host.GPUs) == 0 {
		host.GPUs = cached.GPUs
	}
	if host.LastCheck.IsZero() && !cached.LastCheck.IsZero() {
		host.LastCheck = cached.LastCheck
	}
}
