package sync

import (
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// sourceWorkerLimit leaves at least half the Go CPU budget available to other
// processes, then reduces its share further when the host is already loaded.
// GOMAXPROCS is the environment boundary: recent Go runtimes derive it from
// container CPU quotas as well as the host CPU count.
func sourceWorkerLimit() int {
	return boundedSourceWorkers(runtime.GOMAXPROCS(0), sourceLoadAverage1())
}

func boundedSourceWorkers(procs int, loadAverage float64) int {
	if procs <= 1 {
		return 1
	}
	maximumShare := procs / 2
	if maximumShare < 1 {
		maximumShare = 1
	}
	busy := 0
	if loadAverage > 0 {
		busy = int(math.Ceil(loadAverage))
	}
	available := procs - busy - 1
	if available < 1 {
		return 1
	}
	if available < maximumShare {
		return available
	}
	return maximumShare
}

func sourceLoadAverage1() float64 {
	var text string
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0
		}
		text = string(data)
	case "darwin":
		data, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0
		}
		text = strings.NewReplacer("{", "", "}", "").Replace(string(data))
	default:
		return 0
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
