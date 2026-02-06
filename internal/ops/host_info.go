package ops

import (
	"database/sql"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/hostinfo"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// TryFetchAndCacheHostInfo is like FetchAndCacheHostInfo but returns
// ssh.ErrPoolBusy immediately if all pool slots are occupied.
// Use for periodic/best-effort host info refreshes.
// Returns both the cached info (static fields persisted to DB) and the full
// Host struct (includes dynamic metrics like LoadAvg, MemUsed).
func TryFetchAndCacheHostInfo(database *sql.DB, hostName string, timeout time.Duration) (*db.CachedHostInfo, *hostinfo.Host, error) {
	stdout, stderr, err := ssh.TryRunWithTimeout(hostName, hostinfo.HostInfoCommand, timeout)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, nil, &HostInfoError{Host: hostName, Message: errMsg, Err: err}
	}

	host := hostinfo.ParseHostInfo(stdout)
	host.Name = hostName

	cachedInfo := hostinfo.CachedInfoFromHost(host)
	db.SaveCachedHostInfo(database, cachedInfo)

	return cachedInfo, host, nil
}

// FetchAndCacheHostInfo runs the host info command via SSH, parses the output,
// and saves the result to the DB cache. Returns the cached info or an error.
func FetchAndCacheHostInfo(database *sql.DB, hostName string, timeout time.Duration) (*db.CachedHostInfo, error) {
	stdout, stderr, err := ssh.RunWithTimeout(hostName, hostinfo.HostInfoCommand, timeout)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, &HostInfoError{Host: hostName, Message: errMsg, Err: err}
	}

	host := hostinfo.ParseHostInfo(stdout)
	host.Name = hostName

	cachedInfo := hostinfo.CachedInfoFromHost(host)
	db.SaveCachedHostInfo(database, cachedInfo)

	return cachedInfo, nil
}

// HostStatusResult holds both host info and queue status from a single SSH call.
type HostStatusResult struct {
	HostInfo    *db.CachedHostInfo
	Host        *hostinfo.Host // full host with dynamic metrics (LoadAvg, MemUsed, etc.)
	HostOutput  string         // raw host info output
	ExtraOutput string         // raw extra command output (after separator)
}

// separator between host info and extra command output
const HostStatusSeparator = "---RJ-SECTION---"

// FetchHostStatusCombined runs the host info command combined with an extra
// command in a single SSH call. The extra command's output is returned
// unparsed in ExtraOutput for the caller to interpret.
func FetchHostStatusCombined(database *sql.DB, hostName string, extraCommand string, timeout time.Duration) (*HostStatusResult, error) {
	combined := hostinfo.HostInfoCommand +
		`; echo "` + HostStatusSeparator + `"; ` +
		extraCommand

	stdout, stderr, err := ssh.RunWithTimeout(hostName, combined, timeout)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, &HostInfoError{Host: hostName, Message: errMsg, Err: err}
	}

	result := &HostStatusResult{}

	// Split output at separator
	parts := strings.SplitN(stdout, HostStatusSeparator, 2)

	// Parse host info from first section
	host := hostinfo.ParseHostInfo(parts[0])
	host.Name = hostName
	cachedInfo := hostinfo.CachedInfoFromHost(host)
	db.SaveCachedHostInfo(database, cachedInfo)
	result.HostInfo = cachedInfo
	result.Host = host
	result.HostOutput = parts[0]

	if len(parts) == 2 {
		result.ExtraOutput = strings.TrimSpace(parts[1])
	}

	return result, nil
}

// HostInfoError represents a failure to fetch host info.
type HostInfoError struct {
	Host    string
	Message string
	Err     error
}

func (e *HostInfoError) Error() string {
	return e.Message
}

func (e *HostInfoError) Unwrap() error {
	return e.Err
}
