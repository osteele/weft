package estimate

// ProvisionInput describes the data that must be transferred to provision a
// cloud instance before a job can start.
type ProvisionInput struct {
	ModelDownloadBytes   int64   // total HF model bytes to download
	WorkdirSizeBytes     int64   // workdir sync size; 0 uses default (500 MB)
	UVSyncBytes          int64   // estimated cold uv sync download bytes
	BandwidthBytesPerSec float64 // instance download bandwidth in bytes/sec
}

const defaultWorkdirBytes = 500 * 1024 * 1024 // 500 MB

// EstimateProvision estimates the time to sync the workdir and download models
// onto a cloud instance.
func EstimateProvision(input ProvisionInput) Estimate {
	workdir := input.WorkdirSizeBytes
	if workdir <= 0 {
		workdir = defaultWorkdirBytes
	}

	bw := input.BandwidthBytesPerSec
	if bw <= 0 {
		bw = 1024 * 1024 // 1 MB/s fallback
	}

	sync := TransferTime(workdir, bw)
	download := TransferTime(input.ModelDownloadBytes, bw)
	uvSync := TransferTime(input.UVSyncBytes, bw)
	return sync.Add(download).Add(uvSync)
}
