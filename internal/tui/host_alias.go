package tui

import "github.com/osteele/weft/internal/hostinfo"

type Host = hostinfo.Host
type HostStatus = hostinfo.HostStatus
type QueueCheckStatus = hostinfo.QueueCheckStatus
type GPUInfo = hostinfo.GPUInfo
type JobGPUUsage = hostinfo.JobGPUUsage
type HostRunningJob = hostinfo.HostRunningJob

const (
	HostStatusUnknown  = hostinfo.HostStatusUnknown
	HostStatusChecking = hostinfo.HostStatusChecking
	HostStatusOnline   = hostinfo.HostStatusOnline
	HostStatusOffline  = hostinfo.HostStatusOffline
)

const (
	QueueCheckUnknown  = hostinfo.QueueCheckUnknown
	QueueCheckChecking = hostinfo.QueueCheckChecking
	QueueCheckChecked  = hostinfo.QueueCheckChecked
)

const HostInfoCommand = hostinfo.HostInfoCommand

func ParseHostInfo(output string) *Host {
	return hostinfo.ParseHostInfo(output)
}
