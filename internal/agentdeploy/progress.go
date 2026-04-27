package agentdeploy

import "sync"

// BuildProgressFunc receives coarse build-phase events such as
// "starting fly builder", "syncing source", "building", "downloading".
// It is an alias of EnsureAgentProgressFunc so the build and R2 paths share
// one callback type.
type BuildProgressFunc = EnsureAgentProgressFunc

var (
	buildPhaseMu sync.RWMutex
	buildPhases  = map[string]string{}
)

// SetBuildPhase records the current agent-build phase for a host. Pass an
// empty phase to clear the entry.
func SetBuildPhase(host, phase string) {
	buildPhaseMu.Lock()
	defer buildPhaseMu.Unlock()
	if phase == "" {
		delete(buildPhases, host)
		return
	}
	buildPhases[host] = phase
}

// ActiveBuildPhases returns a snapshot of the in-progress agent builds, keyed
// by host. Returns nil when no builds are running so the common render-path
// case avoids allocating.
func ActiveBuildPhases() map[string]string {
	buildPhaseMu.RLock()
	defer buildPhaseMu.RUnlock()
	if len(buildPhases) == 0 {
		return nil
	}
	out := make(map[string]string, len(buildPhases))
	for k, v := range buildPhases {
		out[k] = v
	}
	return out
}
