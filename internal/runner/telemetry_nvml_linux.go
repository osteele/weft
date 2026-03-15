//go:build linux && cgo

package runner

import (
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type nvmlGPUCollector struct{}

func newNVMLGPUCollector() (gpuTelemetryCollector, bool) {
	ret := nvml.Init()
	return &nvmlGPUCollector{}, ret == nvml.SUCCESS
}

func (c *nvmlGPUCollector) Collect(assignedGPUDevices []string, includeAdvanced bool) ([]TelemetryGPUSample, error) {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, nil
	}

	filter := gpuFilterSet(assignedGPUDevices)
	var samples []TelemetryGPUSample
	for i := 0; i < count; i++ {
		index := strconv.Itoa(i)
		if len(filter) > 0 {
			if _, ok := filter[index]; !ok {
				continue
			}
		}

		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}

		sample := TelemetryGPUSample{GPUIndex: index}
		if name, ret := device.GetName(); ret == nvml.SUCCESS {
			sample.GPUName = strings.TrimSpace(name)
		}
		if mem, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
			sample.GPUMemUsedMiB = int(mem.Used / (1024 * 1024))
		}
		if util, ret := device.GetUtilizationRates(); ret == nvml.SUCCESS {
			value := float64(util.Gpu)
			sample.GPUUtilPct = &value
			memValue := float64(util.Memory)
			sample.GPUMemUtilPct = &memValue
		}
		if includeAdvanced {
			if power, ret := device.GetPowerUsage(); ret == nvml.SUCCESS {
				value := float64(power) / 1000.0
				sample.GPUPowerW = &value
			}
			if tx, ret := device.GetPcieThroughput(nvml.PCIE_UTIL_TX_BYTES); ret == nvml.SUCCESS {
				value := float64(tx) / 1024.0
				sample.GPUPCIeTxMiBS = &value
			}
			if rx, ret := device.GetPcieThroughput(nvml.PCIE_UTIL_RX_BYTES); ret == nvml.SUCCESS {
				value := float64(rx) / 1024.0
				sample.GPUPCIeRxMiBS = &value
			}
			if clock, ret := device.GetClockInfo(nvml.CLOCK_SM); ret == nvml.SUCCESS {
				value := uint32(clock)
				sample.GPUSMClockMHz = &value
			}
			if clock, ret := device.GetClockInfo(nvml.CLOCK_MEM); ret == nvml.SUCCESS {
				value := uint32(clock)
				sample.GPUMemClockMHz = &value
			}
		}

		samples = append(samples, sample)
	}

	return samples, nil
}
