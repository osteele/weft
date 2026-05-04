package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
)

// diskCapSlack is the fraction of requested disk we tolerate as missing.
// Real filesystems lose 3-5% to reserved blocks and journals; 15% gives
// margin for KiB/KB reporting differences and minor overhead before we
// declare the provider silently capped us.
const diskCapSlack = 0.85

// phaseDiskCap is the lifecycle phase string for the infra failure raised
// when the provider delivered less container disk than requested.
const phaseDiskCap = "infra-failure:disk-cap"

type diskCapFailureReport struct {
	TimestampUnix   int64  `json:"timestamp_unix"`
	RequestedDiskGB int    `json:"requested_disk_gb"`
	ActualTotalGB   int    `json:"actual_total_gb"`
	ActualTotal     int64  `json:"actual_total_bytes"`
	ActualFree      int64  `json:"actual_free_bytes"`
	FilesystemPath  string `json:"filesystem_path"`
	DfH             string `json:"df_h"`
}

// checkDiskCap probes the actual mounted disk against what weft asked the
// provider to allocate. If the provider silently delivered substantially
// less, it uploads a disk-cap-failure report to R2 and self-destructs the
// instance with infra_failure rather than letting jobs run for hours and
// hit ENOSPC. Returns true if the instance was terminated.
func checkDiskCap(r2Bucket string, instanceID int64, requestedDiskGB int, diskPath, selfDestructCmd string) bool {
	if requestedDiskGB <= 0 {
		return false
	}
	free, total, err := probeFilesystem(diskPath)
	if err != nil || total <= 0 {
		fmt.Fprintf(os.Stderr, "disk cap probe: cannot stat %s: %v (skipping)\n", diskPath, err)
		return false
	}
	requestedBytes := int64(requestedDiskGB) * 1_000_000_000
	threshold := int64(float64(requestedBytes) * diskCapSlack)
	if total >= threshold {
		fmt.Printf("disk cap probe: ok — requested %d GB, actual %s (%.1f%%)\n",
			requestedDiskGB, formatBytes(total), float64(total)/float64(requestedBytes)*100)
		return false
	}

	report := diskCapFailureReport{
		TimestampUnix:   time.Now().Unix(),
		RequestedDiskGB: requestedDiskGB,
		ActualTotalGB:   int(total / 1_000_000_000),
		ActualTotal:     total,
		ActualFree:      free,
		FilesystemPath:  diskPath,
	}
	if out, err := commandOutput(5*time.Second, "df", "-h", diskPath); err == nil {
		report.DfH = string(out)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	_ = r2Put(r2Bucket, r2keys.InstanceDiskCapFailure(instanceID), string(data))

	msg := fmt.Sprintf("provider delivered %s, weft requested %d GB (<%.0f%%); terminating before workloads run",
		formatBytes(total), requestedDiskGB, diskCapSlack*100)
	fmt.Fprintln(os.Stderr, "disk cap probe: "+msg)
	oplog.Log(oplog.OpPhaseTransition, oplog.WithDetail(phaseDiskCap))

	terminateInstanceWithReason(r2Bucket, instanceID, selfDestructCmd, phaseDiskCap, 0,
		db.TerminationReasonInfraFailure, msg)
	return true
}
