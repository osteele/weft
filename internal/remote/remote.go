package remote

import (
	"context"
	"time"

	"github.com/osteele/remote-jobs/internal/ssh"
)

type ProcessStats = ssh.ProcessStats
type TopProcess = ssh.TopProcess
type JobPIDInfo = ssh.JobPIDInfo
type JobGPUMapping = ssh.JobGPUMapping

func Run(host, command string) (string, string, error) {
	return ssh.Run(host, command)
}

func RunWithContext(ctx context.Context, host, command string) (string, string, error) {
	return ssh.RunWithContext(ctx, host, command)
}

func RunWithTimeout(host, command string, timeout time.Duration) (string, string, error) {
	return ssh.RunWithTimeout(host, command, timeout)
}

func ReadFile(host, path string) (string, error) {
	return ssh.ReadRemoteFile(host, path)
}

func TmuxSessionExists(host, session string) (bool, error) {
	return ssh.TmuxSessionExists(host, session)
}

func TmuxSessionExistsQuick(host, session string) (bool, error) {
	return ssh.TmuxSessionExistsQuick(host, session)
}

func TmuxSessionExistsQuickTimeout(host, session string, timeout time.Duration) (bool, error) {
	return ssh.TmuxSessionExistsQuickTimeout(host, session, timeout)
}

func TmuxKillSession(host, session string) error {
	return ssh.TmuxKillSession(host, session)
}

func GetProcessStats(host, pidFile string) (*ProcessStats, error) {
	return ssh.GetProcessStats(host, pidFile)
}

func GetTopProcesses(host string, limit int) ([]TopProcess, error) {
	return ssh.GetTopProcesses(host, limit)
}

func GetJobGPUMappings(host string, script []byte, jobs []JobPIDInfo) ([]JobGPUMapping, error) {
	return ssh.GetJobGPUMappings(host, script, jobs)
}

func IsConnectionError(output string) bool {
	return ssh.IsConnectionError(output)
}
