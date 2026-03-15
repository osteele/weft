package runner

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/ops"
)

// TelemetryConfig controls high-resolution job telemetry collection.
type TelemetryConfig struct {
	Enabled  bool
	Interval time.Duration
}

type JobTelemetryPolicy struct {
	Interval           time.Duration
	CollectAdvancedGPU bool
}

// DefaultTelemetryConfig enables 1 Hz telemetry by default.
func DefaultTelemetryConfig() TelemetryConfig {
	return TelemetryConfig{
		Enabled:  true,
		Interval: time.Second,
	}
}

func DefaultJobTelemetryPolicy() JobTelemetryPolicy {
	return JobTelemetryPolicy{
		Interval:           time.Second,
		CollectAdvancedGPU: true,
	}
}

func BenchmarkTelemetryPolicy() JobTelemetryPolicy {
	return JobTelemetryPolicy{
		Interval:           5 * time.Second,
		CollectAdvancedGPU: false,
	}
}

func TelemetryPolicyForJob(job *ops.CommandJob) JobTelemetryPolicy {
	if job != nil && HasTag(job, "benchmark") {
		return BenchmarkTelemetryPolicy()
	}
	return DefaultJobTelemetryPolicy()
}

// TelemetrySample holds a richer per-sample telemetry record for export.
type TelemetrySample struct {
	Ts               int64                `json:"ts"`
	ElapsedS         float64              `json:"elapsed_s,omitempty"`
	ProcCPUUserS     float64              `json:"proc_cpu_user_s,omitempty"`
	ProcCPUSysS      float64              `json:"proc_cpu_sys_s,omitempty"`
	ProcRSSKB        int64                `json:"proc_rss_kb,omitempty"`
	HostCPUUtilPct   *float64             `json:"host_cpu_util_pct,omitempty"`
	ProcDiskReadBPS  *float64             `json:"proc_disk_read_bps,omitempty"`
	ProcDiskWriteBPS *float64             `json:"proc_disk_write_bps,omitempty"`
	ProcNetRxBPS     *float64             `json:"proc_net_rx_bps,omitempty"`
	ProcNetTxBPS     *float64             `json:"proc_net_tx_bps,omitempty"`
	GPUs             []TelemetryGPUSample `json:"gpus,omitempty"`
}

// TelemetryGPUSample holds one GPU's telemetry for a timestamp.
type TelemetryGPUSample struct {
	GPUIndex       string   `json:"gpu_index"`
	GPUName        string   `json:"gpu_name,omitempty"`
	GPUMemUsedMiB  int      `json:"gpu_mem_used_mib,omitempty"`
	GPUUtilPct     *float64 `json:"gpu_util_pct,omitempty"`
	GPUMemUtilPct  *float64 `json:"gpu_mem_util_pct,omitempty"`
	GPUPowerW      *float64 `json:"gpu_power_w,omitempty"`
	GPUPCIeTxMiBS  *float64 `json:"gpu_pcie_tx_mib_s,omitempty"`
	GPUPCIeRxMiBS  *float64 `json:"gpu_pcie_rx_mib_s,omitempty"`
	GPUSMClockMHz  *uint32  `json:"gpu_sm_clock_mhz,omitempty"`
	GPUMemClockMHz *uint32  `json:"gpu_mem_clock_mhz,omitempty"`
}

type gpuTelemetryCollector interface {
	Collect(assignedGPUDevices []string, includeAdvanced bool) ([]TelemetryGPUSample, error)
}

var defaultGPUTelemetryCollector gpuTelemetryCollector = &autoGPUTelemetryCollector{}

type autoGPUTelemetryCollector struct {
	once          sync.Once
	nvmlCollector gpuTelemetryCollector
	fallback      *nvidiaSmiGPUCollector
}

func (c *autoGPUTelemetryCollector) Collect(assignedGPUDevices []string, includeAdvanced bool) ([]TelemetryGPUSample, error) {
	c.once.Do(func() {
		if collector, ok := newNVMLGPUCollector(); ok {
			c.nvmlCollector = collector
		}
		c.fallback = &nvidiaSmiGPUCollector{}
	})

	if c.nvmlCollector != nil {
		samples, err := c.nvmlCollector.Collect(assignedGPUDevices, includeAdvanced)
		if err == nil {
			return samples, nil
		}
	}

	return c.fallback.Collect(assignedGPUDevices, includeAdvanced)
}

type nvidiaSmiGPUCollector struct{}

func (c *nvidiaSmiGPUCollector) Collect(assignedGPUDevices []string, includeAdvanced bool) ([]TelemetryGPUSample, error) {
	rows := queryNvidiaSmi("--query-gpu=index,name,utilization.gpu,utilization.memory,memory.used,power.draw,clocks.current.sm,clocks.current.memory", 8)
	if rows == nil {
		return nil, nil
	}

	filter := gpuFilterSet(assignedGPUDevices)
	var samples []TelemetryGPUSample
	for _, parts := range rows {
		index := strings.TrimSpace(parts[0])
		if len(filter) > 0 {
			if _, ok := filter[index]; !ok {
				continue
			}
		}

		sample := TelemetryGPUSample{
			GPUIndex:      index,
			GPUName:       strings.TrimSpace(parts[1]),
			GPUMemUsedMiB: parseNvidiaSmiInt(parts[4]),
		}
		sample.GPUUtilPct = parseNvidiaSmiFloatPtr(parts[2])
		sample.GPUMemUtilPct = parseNvidiaSmiFloatPtr(parts[3])
		if includeAdvanced {
			sample.GPUPowerW = parseNvidiaSmiFloatPtr(parts[5])
			sample.GPUSMClockMHz = parseNvidiaSmiUint32Ptr(parts[6])
			sample.GPUMemClockMHz = parseNvidiaSmiUint32Ptr(parts[7])
		}
		samples = append(samples, sample)
	}

	return samples, nil
}

// WriteTelemetrySample appends one richer telemetry sample to the telemetry JSONL file.
func WriteTelemetrySample(paths JobPaths, sample TelemetrySample) error {
	f, err := os.OpenFile(paths.Telemetry, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

func buildTelemetrySample(now time.Time, pid, pgid int, rs *RunningJobState) TelemetrySample {
	rusagePID := pid
	if pgid > 0 {
		rusagePID = pgid
	}

	var sample TelemetrySample
	sample.Ts = now.Unix()
	if rs.StartedAt > 0 {
		sample.ElapsedS = now.Sub(time.Unix(rs.StartedAt, 0)).Seconds()
	}

	if _, err := os.Stat("/proc"); err == nil {
		userTicks, sysTicks, _ := ProcResourceUsage(rusagePID)
		sample.ProcCPUUserS = parseFloatOrZero(TicksToSeconds(userTicks))
		sample.ProcCPUSysS = parseFloatOrZero(TicksToSeconds(sysTicks))
	}
	sample.ProcRSSKB = ProcCurrentRSSKB(rusagePID)

	readBytes, writeBytes := ProcIOBytes(rusagePID)
	if rs.TelemetryLastSampleAt > 0 {
		elapsed := now.Sub(time.Unix(rs.TelemetryLastSampleAt, 0)).Seconds()
		if elapsed > 0 {
			if value := bytesPerSecond(readBytes, rs.TelemetryLastReadBytes, elapsed); value != nil {
				sample.ProcDiskReadBPS = value
			}
			if value := bytesPerSecond(writeBytes, rs.TelemetryLastWriteBytes, elapsed); value != nil {
				sample.ProcDiskWriteBPS = value
			}
		}
	}
	rs.TelemetryLastSampleAt = now.Unix()
	rs.TelemetryLastReadBytes = readBytes
	rs.TelemetryLastWriteBytes = writeBytes

	if gpuSamples, err := defaultGPUTelemetryCollector.Collect(rs.GPUDevices, rs.TelemetryAdvancedGPU); err == nil && len(gpuSamples) > 0 {
		sample.GPUs = gpuSamples
	}

	return sample
}

func gpuFilterSet(indices []string) map[string]struct{} {
	if len(indices) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(indices))
	for _, index := range indices {
		index = strings.TrimSpace(index)
		if index != "" {
			set[index] = struct{}{}
		}
	}
	return set
}

func parseFloatOrZero(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseNvidiaSmiInt(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(value, " W")))
	if err != nil {
		return 0
	}
	return n
}

func parseNvidiaSmiFloatPtr(value string) *float64 {
	value = strings.TrimSpace(strings.TrimSuffix(value, " W"))
	if value == "" || strings.EqualFold(value, "N/A") {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func parseNvidiaSmiUint32Ptr(value string) *uint32 {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "N/A") {
		return nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return nil
	}
	v := uint32(parsed)
	return &v
}

func bytesPerSecond(current, previous uint64, elapsed float64) *float64 {
	if elapsed <= 0 || current < previous {
		return nil
	}
	value := float64(current-previous) / elapsed
	return &value
}
