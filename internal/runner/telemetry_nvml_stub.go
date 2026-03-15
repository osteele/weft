//go:build !linux || !cgo

package runner

func newNVMLGPUCollector() (gpuTelemetryCollector, bool) {
	return nil, false
}
