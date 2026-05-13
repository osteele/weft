// Package transferbw tracks transfer bandwidth using an exponential moving
// average (EMA) keyed by (source_endpoint, dest_endpoint). Observations come
// from prestage transfers; estimates feed placement scoring.
//
// Each endpoint is one of:
//   - "hf"                            — Hugging Face Hub (source only)
//   - "r2"                            — Cloudflare R2 (source only currently)
//   - "donor:<instance_id>"           — donor-class cloud fan-out source
//   - "cloud:<provider>:<datacenter>" — e.g., "cloud:vastai:us-east-1"
//   - "onprem:<hostname>"             — e.g., "onprem:cool30"
//
// Raw observations are stored with full provenance (cloud instance IDs, etc.)
// so that EMA estimates can be recomputed if we later change the grouping key
// (e.g., adding advertised-bandwidth buckets).
package transferbw

import (
	"database/sql"
	"fmt"
	"time"
)

// Alpha is the EMA smoothing factor. Higher values weight recent observations
// more heavily. 0.3 gives ~90% decay over ~6 observations.
const Alpha = 0.3

// Endpoint classifies one side of a transfer.
type Endpoint struct {
	Kind       string // "hf", "r2", "cloud", "onprem"
	Provider   string // cloud only: "vastai", "runpod"
	Datacenter string // cloud only: e.g., "us-east-1"
	Hostname   string // onprem only: e.g., "cool30"
	InstanceID string // cloud only: provider instance ID (for provenance)
}

// Key returns the canonical grouping key for an endpoint.
// Instance-level detail is intentionally excluded — it's stored in the
// observations table for future re-keying.
func (e Endpoint) Key() string {
	switch e.Kind {
	case "hf", "r2":
		return e.Kind
	case "cloud":
		return fmt.Sprintf("cloud:%s:%s", e.Provider, e.Datacenter)
	case "onprem":
		return fmt.Sprintf("onprem:%s", e.Hostname)
	case "donor":
		return fmt.Sprintf("donor:%s", e.InstanceID)
	default:
		return e.Kind
	}
}

// HFEndpoint returns an endpoint for Hugging Face Hub.
func HFEndpoint() Endpoint { return Endpoint{Kind: "hf"} }

// R2Endpoint returns an endpoint for Cloudflare R2.
func R2Endpoint() Endpoint { return Endpoint{Kind: "r2"} }

// CloudEndpoint returns an endpoint for a cloud instance.
func CloudEndpoint(provider, datacenter, instanceID string) Endpoint {
	return Endpoint{Kind: "cloud", Provider: provider, Datacenter: datacenter, InstanceID: instanceID}
}

// OnPremEndpoint returns an endpoint for an on-premises host.
func OnPremEndpoint(hostname string) Endpoint {
	return Endpoint{Kind: "onprem", Hostname: hostname}
}

// DonorEndpoint returns an endpoint for donor-class cloud fan-out transfers.
func DonorEndpoint(instanceID string) Endpoint {
	return Endpoint{Kind: "donor", InstanceID: instanceID}
}

// BandwidthEstimate holds the computed bandwidth for a (source, dest) pair.
type BandwidthEstimate struct {
	EMABytesPerSec   float64
	EMAVariance      float64
	ObservationCount int
}

// InitSchema creates the transfer_observations table if it doesn't exist.
func InitSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS transfer_observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			dest_key TEXT NOT NULL,
			source_instance_id TEXT,
			dest_instance_id TEXT,
			bytes_transferred INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			observed_bw_bps REAL NOT NULL,
			created_at INTEGER NOT NULL
		)
	`)
	return err
}

// RecordObservation appends a raw transfer observation.
// No-op if duration or bytes are non-positive.
func RecordObservation(db *sql.DB, source, dest Endpoint, bytesTransferred int64, duration time.Duration) error {
	if duration <= 0 || bytesTransferred <= 0 {
		return nil
	}

	observedBW := float64(bytesTransferred) / duration.Seconds()
	now := time.Now().Unix()

	_, err := db.Exec(`
		INSERT INTO transfer_observations
			(source_key, dest_key, source_instance_id, dest_instance_id,
			 bytes_transferred, duration_ms, observed_bw_bps, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, source.Key(), dest.Key(), source.InstanceID, dest.InstanceID,
		bytesTransferred, duration.Milliseconds(), observedBW, now)
	return err
}

// ComputeBandwidth computes the EMA bandwidth for a (source, dest) key pair
// from stored observations (oldest first). Returns nil if no observations exist.
func ComputeBandwidth(db *sql.DB, sourceKey, destKey string) (*BandwidthEstimate, error) {
	rows, err := db.Query(
		`SELECT observed_bw_bps FROM transfer_observations
		 WHERE source_key = ? AND dest_key = ?
		 ORDER BY created_at ASC`, sourceKey, destKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return emaFromRows(rows)
}

// ComputeBandwidthToDest computes the EMA bandwidth across all sources for a
// given destination. Used in placement scoring where the specific source is
// not yet known.
func ComputeBandwidthToDest(db *sql.DB, destKey string) (*BandwidthEstimate, error) {
	rows, err := db.Query(
		`SELECT observed_bw_bps FROM transfer_observations
		 WHERE dest_key = ?
		 ORDER BY created_at ASC`, destKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return emaFromRows(rows)
}

func emaFromRows(rows *sql.Rows) (*BandwidthEstimate, error) {
	var ema, variance float64
	var count int
	for rows.Next() {
		var bw float64
		if err := rows.Scan(&bw); err != nil {
			return nil, err
		}
		count++
		if count == 1 {
			ema = bw
			variance = 0
		} else {
			ema = Alpha*bw + (1-Alpha)*ema
			diff := bw - ema
			variance = Alpha*diff*diff + (1-Alpha)*variance
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &BandwidthEstimate{
		EMABytesPerSec:   ema,
		EMAVariance:      variance,
		ObservationCount: count,
	}, nil
}
