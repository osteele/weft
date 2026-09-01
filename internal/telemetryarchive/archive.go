// Package telemetryarchive moves terminal raw telemetry out of operational
// SQLite while retaining verified object metadata and compact summaries.
package telemetryarchive

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
)

type RemoteCopy struct {
	R2Key string
	ETag  string
}

func EncodeTimeseries(samples []db.TimeseriesSample) ([]byte, error) {
	return encodeJSONL(samples)
}

func EncodeRich(samples []db.TelemetrySample) ([]byte, error) {
	return encodeJSONL(samples)
}

func encodeJSONL[T any](values []T) ([]byte, error) {
	var out strings.Builder
	enc := json.NewEncoder(&out)
	for _, value := range values {
		if err := enc.Encode(value); err != nil {
			return nil, err
		}
	}
	return []byte(out.String()), nil
}

func validateJSONL[T any](raw []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Telemetry lines can include many GPUs; retain a generous explicit bound
	// and fail visibly instead of accepting Scanner's default truncation.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return fmt.Errorf("invalid telemetry JSONL line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read telemetry JSONL: %w", err)
	}
	return nil
}

// FinalizeTimeseries stores and verifies raw bytes before recording the
// summary/object receipt and pruning relational samples. Repeating the call is
// safe; every step is content-addressed or idempotent.
func FinalizeTimeseries(database *sql.DB, jobID, runID int64, raw []byte, samples []db.TimeseriesSample, remote RemoteCopy) error {
	if len(raw) == 0 || len(samples) == 0 || runID <= 0 {
		return fmt.Errorf("timeseries archive requires a run and non-empty raw samples")
	}
	if err := validateJSONL[db.TimeseriesSample](raw); err != nil {
		return err
	}
	stored, size, digest, err := artifacts.CaptureSystemBlob(db.TimeseriesRawKind, raw)
	if err != nil {
		return fmt.Errorf("capture raw timeseries: %w", err)
	}
	if err := db.UpsertTimeseriesSummary(database, db.SummarizeTimeseries(jobID, runID, samples)); err != nil {
		return err
	}
	if err := db.UpsertRawTelemetryObject(database, db.RawTelemetryObject{
		JobID: jobID, AttemptID: runID, Kind: db.TimeseriesRawKind,
		R2Key: remote.R2Key, StoredPath: stored, SizeBytes: size, SHA256: digest,
		ETag: remote.ETag, SchemaVersion: 1,
	}); err != nil {
		return err
	}
	return db.DeleteTimeseriesByRun(database, runID)
}

// FinalizeRich stores and verifies raw rich telemetry before recording the
// per-attempt rollup/object receipt and pruning both relational tables.
func FinalizeRich(database *sql.DB, jobID, runID int64, raw []byte, rollup *db.RichTelemetryRollup, remote RemoteCopy) error {
	if len(raw) == 0 || rollup == nil || rollup.SampleCount == 0 || runID <= 0 {
		return fmt.Errorf("rich telemetry archive requires a run and non-empty raw samples")
	}
	if err := validateJSONL[db.TelemetrySample](raw); err != nil {
		return err
	}
	stored, size, digest, err := artifacts.CaptureSystemBlob(db.TelemetryRawKind, raw)
	if err != nil {
		return fmt.Errorf("capture raw telemetry: %w", err)
	}
	if err := db.UpsertRichTelemetryRollup(database, rollup); err != nil {
		return err
	}
	if err := db.UpsertRawTelemetryObject(database, db.RawTelemetryObject{
		JobID: jobID, AttemptID: runID, Kind: db.TelemetryRawKind,
		R2Key: remote.R2Key, StoredPath: stored, SizeBytes: size, SHA256: digest,
		ETag: remote.ETag, SchemaVersion: 1,
	}); err != nil {
		return err
	}
	return db.DeleteTelemetryByRun(database, runID)
}
