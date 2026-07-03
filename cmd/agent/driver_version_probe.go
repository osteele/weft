package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/compat"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
)

// phaseDriverTooOld is the lifecycle phase string for the infra failure raised
// when the host's NVIDIA driver is below what the campaign's wheels need.
const phaseDriverTooOld = "infra-failure:driver-too-old"

type driverFailureReport struct {
	TimestampUnix   int64  `json:"timestamp_unix"`
	RequiredMajor   int    `json:"required_driver_major"`
	ActualMajor     int    `json:"actual_driver_major"`
	ActualVersion   string `json:"actual_driver_version"`
	NvidiaSmiOutput string `json:"nvidia_smi_query_output,omitempty"`
}

// checkDriverVersion probes the host's NVIDIA driver via nvidia-smi and
// compares it to the major version weft inferred at placement time. If the
// host's driver is too old, it uploads a structured failure report to R2 and
// self-destructs the instance with infra_failure rather than letting jobs run
// up to several minutes of setup before failing with cuda_driver_too_old.
//
// Returns true if the instance was terminated. Returns false (with a logged
// warning) when nvidia-smi is unavailable or unparseable — failing closed on
// "driver check tooling broken" would block CPU-only / non-GPU instances and
// hosts where nvidia-smi version differences cause parse misses.
func checkDriverVersion(r2Bucket string, instanceID int64, requiredMajor int, selfDestructCmd string) bool {
	if requiredMajor <= 0 {
		return false
	}
	out, err := commandOutput(5*time.Second, "nvidia-smi",
		"--query-gpu=driver_version", "--format=csv,noheader")
	if err != nil {
		fmt.Fprintf(os.Stderr, "driver probe: nvidia-smi failed: %v (skipping check)\n", err)
		return false
	}
	version, major, ok := parseDriverMajor(out)
	if !ok {
		fmt.Fprintf(os.Stderr, "driver probe: cannot parse nvidia-smi output %q (skipping check)\n", out)
		return false
	}
	// Driver-major link of the CUDA compatibility chain — same comparison
	// semantics as the launch-time offer filter and instance-reuse checks
	// (specs/campaign-lifecycle.allium contract CUDACompatibilityChain).
	if compat.ValidateCUDAChain(compat.CUDAChain{
		MinDriverMajor: requiredMajor,
		DriverMajor:    major,
	}) == nil {
		fmt.Printf("driver probe: ok — required %d, actual %s\n", requiredMajor, version)
		return false
	}

	report := driverFailureReport{
		TimestampUnix:   time.Now().Unix(),
		RequiredMajor:   requiredMajor,
		ActualMajor:     major,
		ActualVersion:   version,
		NvidiaSmiOutput: out,
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	_ = r2Put(r2Bucket, r2keys.InstanceDriverFailure(instanceID), string(data))

	msg := fmt.Sprintf("host driver %s (major %d) is below required major %d; terminating before workloads run",
		version, major, requiredMajor)
	fmt.Fprintln(os.Stderr, "driver probe: "+msg)
	oplog.Log(oplog.OpPhaseTransition, oplog.WithDetail(phaseDriverTooOld))

	terminateInstanceWithReason(r2Bucket, instanceID, selfDestructCmd, phaseDriverTooOld, 0,
		db.TerminationReasonInfraFailure, msg)
	return true
}

// nvidiaSmiVersionRe matches the leading numeric major.minor.patch from
// nvidia-smi --query-gpu=driver_version output. Multi-GPU hosts return one
// row per device but the driver is host-wide, so the first row suffices.
var nvidiaSmiVersionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// parseDriverMajor extracts the driver version string and integer major from
// nvidia-smi --query-gpu=driver_version output (e.g. "570.133.07" → "570.133.07", 570).
// Returns ok=false when no numeric major can be found.
func parseDriverMajor(out string) (version string, major int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := nvidiaSmiVersionRe.FindString(line)
		if m == "" {
			continue
		}
		n, err := strconv.Atoi(strings.SplitN(m, ".", 2)[0])
		if err != nil {
			continue
		}
		return line, n, true
	}
	return "", 0, false
}
