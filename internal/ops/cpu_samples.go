package ops

import (
	"database/sql"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

func updateJobCPUSamples(database *sql.DB, job *db.Job, timeout time.Duration) (bool, error) {
	if job == nil {
		return false, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	samples, err := queueRemoteClient.Samples(job.Host, job.ID, timeout)
	if err != nil {
		return false, nil
	}
	stats, err := parseCPUSamples(samples)
	if err != nil || stats == nil {
		return false, nil
	}

	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if cpuStatsEqual(meta.CPU, stats) {
		return false, nil
	}
	meta.CPU = stats
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		return false, err
	}
	job.Metadata = meta
	return true, nil
}

func parseCPUSamples(samples string) (*db.JobCPUStats, error) {
	lines := strings.Split(samples, "\n")
	var count int
	var mean float64
	var m2 float64
	var max float64
	var latest float64

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		latest = value
		if count == 0 || value > max {
			max = value
		}
		count++
		delta := value - mean
		mean += delta / float64(count)
		m2 += delta * (value - mean)
	}

	if count == 0 {
		return nil, nil
	}

	stddev := math.Sqrt(m2 / float64(count))
	return &db.JobCPUStats{
		Latest:  floatPtr(latest),
		Max:     floatPtr(max),
		Mean:    floatPtr(mean),
		Stddev:  floatPtr(stddev),
		Samples: count,
	}, nil
}

func cpuStatsEqual(a, b *db.JobCPUStats) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Samples != b.Samples {
		return false
	}
	return floatPtrEqual(a.Latest, b.Latest) &&
		floatPtrEqual(a.Max, b.Max) &&
		floatPtrEqual(a.Mean, b.Mean) &&
		floatPtrEqual(a.Stddev, b.Stddev)
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func floatPtr(value float64) *float64 {
	return &value
}
