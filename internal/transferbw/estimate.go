package transferbw

import "database/sql"

// MinObservations is the minimum number of observations before we trust the
// EMA over the static inventory value.
const MinObservations = 2

// EffectiveBandwidth returns the learned EMA bandwidth for a (source, dest)
// key pair if enough observations exist, otherwise falls back to staticBW
// (from host inventory YAML).
func EffectiveBandwidth(db *sql.DB, sourceKey, destKey string, staticBW float64) float64 {
	est, err := ComputeBandwidth(db, sourceKey, destKey)
	if err != nil || est == nil || est.ObservationCount < MinObservations {
		return staticBW
	}
	return est.EMABytesPerSec
}

// EffectiveBandwidthToDest returns the learned EMA bandwidth across all sources
// for a given destination. Used in placement scoring where the specific source
// is not yet known. Falls back to staticBW if insufficient observations.
func EffectiveBandwidthToDest(db *sql.DB, destKey string, staticBW float64) (bw float64, observationCount int) {
	est, err := ComputeBandwidthToDest(db, destKey)
	if err != nil || est == nil || est.ObservationCount < MinObservations {
		return staticBW, 0
	}
	return est.EMABytesPerSec, est.ObservationCount
}

// ObservationCount returns the number of observations for a (source, dest)
// key pair, or 0 on error.
func ObservationCount(db *sql.DB, sourceKey, destKey string) int {
	est, err := ComputeBandwidth(db, sourceKey, destKey)
	if err != nil || est == nil {
		return 0
	}
	return est.ObservationCount
}
