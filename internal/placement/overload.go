package placement

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type HostLoadState string

const (
	HostLoadNormal     HostLoadState = "normal"
	HostLoadBusy       HostLoadState = "busy"
	HostLoadOverloaded HostLoadState = "overloaded"
)

type HostLoadOptions struct {
	BusyGPUPct       int
	BusyCPUPct       int
	BusyRAMPct       int
	OverloadedGPUPct int
	OverloadedCPUPct int
	OverloadedRAMPct int
	RecentWindow     time.Duration
	RecentSamples    int
}

type HostLoadAssessment struct {
	Host   string
	State  HostLoadState
	Reason string
}

func DefaultHostLoadOptions() HostLoadOptions {
	return HostLoadOptions{
		BusyGPUPct:       85,
		BusyCPUPct:       85,
		BusyRAMPct:       85,
		OverloadedGPUPct: 98,
		OverloadedCPUPct: 98,
		OverloadedRAMPct: 96,
		RecentWindow:     10 * time.Minute,
		RecentSamples:    3,
	}
}

func AssessHostLoad(database *sql.DB, host string, metrics *HostMetrics, opts HostLoadOptions) HostLoadAssessment {
	if opts.BusyGPUPct <= 0 {
		opts = DefaultHostLoadOptions()
	}
	host = strings.TrimSpace(host)
	assessment := HostLoadAssessment{Host: host, State: HostLoadNormal}
	if metrics != nil {
		state, reason := classifyHostMetrics(metrics, opts)
		assessment.State = state
		assessment.Reason = reason
		if state == HostLoadOverloaded {
			return assessment
		}
	}
	if database == nil || host == "" {
		return assessment
	}
	recentState, recentReason := recentHostLoadState(database, host, opts)
	if moreSevereLoadState(recentState, assessment.State) {
		assessment.State = recentState
		assessment.Reason = recentReason
	}
	return assessment
}

func AssessHostsLoad(database *sql.DB, metrics map[string]*HostMetrics, opts HostLoadOptions) map[string]HostLoadAssessment {
	out := make(map[string]HostLoadAssessment, len(metrics))
	for host, m := range metrics {
		assessment := AssessHostLoad(database, host, m, opts)
		if assessment.State != HostLoadNormal {
			out[host] = assessment
		}
	}
	return out
}

func OverloadedHostsFromRecent(database *sql.DB, hosts []string) map[string]HostLoadAssessment {
	out := map[string]HostLoadAssessment{}
	opts := DefaultHostLoadOptions()
	for _, host := range hosts {
		assessment := AssessHostLoad(database, host, nil, opts)
		if assessment.State == HostLoadOverloaded {
			out[host] = assessment
		}
	}
	return out
}

func classifyHostMetrics(m *HostMetrics, opts HostLoadOptions) (HostLoadState, string) {
	if m == nil {
		return HostLoadNormal, ""
	}
	if m.GPUPercent >= opts.OverloadedGPUPct && opts.OverloadedGPUPct > 0 {
		return HostLoadOverloaded, fmt.Sprintf("GPU %d%%", m.GPUPercent)
	}
	if m.CPUPercent >= opts.OverloadedCPUPct && opts.OverloadedCPUPct > 0 {
		return HostLoadOverloaded, fmt.Sprintf("CPU %d%%", m.CPUPercent)
	}
	if m.RAMPercent >= opts.OverloadedRAMPct && opts.OverloadedRAMPct > 0 {
		return HostLoadOverloaded, fmt.Sprintf("RAM %d%%", m.RAMPercent)
	}
	if m.GPUPercent >= opts.BusyGPUPct && opts.BusyGPUPct > 0 {
		return HostLoadBusy, fmt.Sprintf("GPU %d%%", m.GPUPercent)
	}
	if m.CPUPercent >= opts.BusyCPUPct && opts.BusyCPUPct > 0 {
		return HostLoadBusy, fmt.Sprintf("CPU %d%%", m.CPUPercent)
	}
	if m.RAMPercent >= opts.BusyRAMPct && opts.BusyRAMPct > 0 {
		return HostLoadBusy, fmt.Sprintf("RAM %d%%", m.RAMPercent)
	}
	return HostLoadNormal, ""
}

func recentHostLoadState(database *sql.DB, host string, opts HostLoadOptions) (HostLoadState, string) {
	if opts.RecentSamples <= 0 {
		opts.RecentSamples = 1
	}
	if opts.RecentWindow <= 0 {
		opts.RecentWindow = 10 * time.Minute
	}
	cutoff := time.Now().Add(-opts.RecentWindow).Unix()
	rows, err := database.Query(`
		SELECT gpu_pct, cpu_pct
		  FROM host_contention_obs
		 WHERE host = ? AND observed_at >= ?
		 ORDER BY observed_at DESC
		 LIMIT ?`, host, cutoff, opts.RecentSamples)
	if err != nil {
		return HostLoadNormal, ""
	}
	defer rows.Close()

	count := 0
	gpuOver := 0
	cpuOver := 0
	for rows.Next() {
		var gpuPct, cpuPct sql.NullInt64
		if err := rows.Scan(&gpuPct, &cpuPct); err != nil {
			return HostLoadNormal, ""
		}
		count++
		if gpuPct.Valid && int(gpuPct.Int64) >= opts.OverloadedGPUPct {
			gpuOver++
		}
		if cpuPct.Valid && int(cpuPct.Int64) >= opts.OverloadedCPUPct {
			cpuOver++
		}
	}
	if count < opts.RecentSamples {
		return HostLoadNormal, ""
	}
	switch {
	case gpuOver == count:
		return HostLoadOverloaded, fmt.Sprintf("GPU >=%d%% for %d samples", opts.OverloadedGPUPct, count)
	case cpuOver == count:
		return HostLoadOverloaded, fmt.Sprintf("CPU >=%d%% for %d samples", opts.OverloadedCPUPct, count)
	default:
		return HostLoadNormal, ""
	}
}

func moreSevereLoadState(a, b HostLoadState) bool {
	return hostLoadRank(a) > hostLoadRank(b)
}

func hostLoadRank(s HostLoadState) int {
	switch s {
	case HostLoadOverloaded:
		return 2
	case HostLoadBusy:
		return 1
	default:
		return 0
	}
}
