package ops

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
)

type launchHeartbeatSample struct {
	Ts             int64 `json:"ts"`
	GPUUtilPct     int   `json:"gpu_util_pct"`
	GPUMemUsedMiB  int   `json:"gpu_mem_used_mib"`
	GPUMemTotalMiB int   `json:"gpu_mem_total_mib"`
	GPUTempC       int   `json:"gpu_temp_c"`
	HostRSSKB      int64 `json:"host_rss_kb"`
	HostMemTotalKB int64 `json:"host_mem_total_kb"`
	DiskFreeBytes  int64 `json:"disk_free_bytes"`
	DiskTotalBytes int64 `json:"disk_total_bytes"`
}

// FetchLaunchHostStatusFromDB builds rental-host status from local DB state
// without SSH access.
func FetchLaunchHostStatusFromDB(database *sql.DB, hostName string) (*HostStatusResult, error) {
	launchID, ok := parseLaunchHostID(hostName)
	if !ok {
		return nil, fmt.Errorf("invalid rental host: %s", hostName)
	}

	launch, err := db.GetLaunch(database, launchID)
	if err != nil {
		return nil, err
	}
	if launch == nil {
		return nil, fmt.Errorf("launch not found for host %s", hostName)
	}

	host := &hostinfo.Host{
		Name:        hostName,
		LastCheck:   time.Now(),
		QueueStatus: hostinfo.QueueCheckUnknown,
	}
	switch launch.Status {
	case db.LaunchStatusRunning, db.LaunchStatusGrace:
		host.Status = hostinfo.HostStatusOnline
	case db.LaunchStatusLaunching, db.LaunchStatusPlanned:
		host.Status = hostinfo.HostStatusChecking
	default:
		host.Status = hostinfo.HostStatusOffline
		host.Error = "instance " + launch.Status
	}

	if cached, err := db.LoadCachedHostInfo(database, hostName); err == nil && cached != nil {
		hostinfo.UpdateHostWithCachedStatic(host, hostinfo.HostFromCachedInfo(cached))
	}
	if host.Arch == "" {
		host.Arch = strings.TrimSpace(launch.Provider)
	}
	if host.Model == "" {
		host.Model = launch.DisplayGPUBrief()
	}

	if live, err := db.GetLaunchLiveState(database, launchID); err == nil && live != nil {
		applyLaunchLiveState(host, live, launch)
	}

	return &HostStatusResult{
		HostInfo: hostinfo.CachedInfoFromHost(host),
		Host:     host,
	}, nil
}

func applyLaunchLiveState(host *hostinfo.Host, live *db.LaunchLiveState, launch *db.Launch) {
	if host == nil || live == nil {
		return
	}

	var hb launchHeartbeatSample
	if strings.TrimSpace(live.HeartbeatJSON) != "" {
		_ = json.Unmarshal([]byte(live.HeartbeatJSON), &hb)
	}

	if hb.Ts > 0 {
		host.LastCheck = time.Unix(hb.Ts, 0)
	}
	if hb.HostMemTotalKB > 0 {
		host.MemTotal = formatKBAsG(hb.HostMemTotalKB)
	}
	if hb.HostRSSKB > 0 {
		host.MemUsed = formatKBAsG(hb.HostRSSKB)
	}
	if hb.DiskFreeBytes > 0 {
		host.DiskFree = hb.DiskFreeBytes
	}
	if hb.DiskTotalBytes > 0 {
		host.DiskTotal = hb.DiskTotalBytes
	}

	if hb.GPUMemTotalMiB > 0 || hb.GPUUtilPct > 0 || hb.GPUTempC > 0 {
		name := ""
		if launch != nil {
			name = launch.DisplayGPUBrief()
		}
		if name == "" {
			name = "GPU"
		}
		host.GPUs = []hostinfo.GPUInfo{{
			Index:       0,
			Name:        name,
			Temperature: hb.GPUTempC,
			Utilization: hb.GPUUtilPct,
			MemUsed:     fmt.Sprintf("%d MiB", maxInt(hb.GPUMemUsedMiB, 0)),
			MemTotal:    fmt.Sprintf("%d MiB", maxInt(hb.GPUMemTotalMiB, 0)),
		}}
	}
}

func parseLaunchHostID(host string) (int64, bool) {
	if !db.IsLaunchHost(host) {
		return 0, false
	}
	parts := strings.SplitN(host, ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func formatKBAsG(kb int64) string {
	if kb <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1fG", float64(kb)/(1024.0*1024.0))
}

func maxInt(v, floor int) int {
	if v < floor {
		return floor
	}
	return v
}
