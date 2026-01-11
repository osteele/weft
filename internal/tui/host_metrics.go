package tui

import "github.com/osteele/remote-jobs/internal/hostinfo"

func hostCPULoadPercent(host *Host) (int, bool) {
	return hostinfo.HostCPULoadPercent(host)
}

func hostMemUsagePercent(host *Host) (int, bool) {
	return hostinfo.HostMemUsagePercent(host)
}

func hostGPULoadPercent(host *Host) (int, bool) {
	return hostinfo.HostGPULoadPercent(host)
}
