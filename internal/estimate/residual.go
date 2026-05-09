package estimate

import (
	"database/sql"
	"math"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/predictor"
)

const residualMinSamples = 5
const residualHalfLife = 14 * 24 * time.Hour

type residualGroup struct {
	source string
	factor float64
	n      int
}

// ApplyResidualCorrection adjusts duration predictions using recent local
// actual/predicted ratios. It is intentionally conservative and no-ops until
// enough matching completed runs exist.
func ApplyResidualCorrection(jobID int64, project, gpuClass, command string, pred *predictor.Result) {
	if pred == nil || pred.DurationS == nil {
		return
	}
	group, ok := lookupResidualCorrection(project, gpuClass, command)
	if !ok {
		return
	}
	scalePrediction(pred.DurationS, group.factor)
	if pred.DurationMetadata == nil {
		pred.DurationMetadata = &predictor.RuntimeMetadata{}
	}
	pred.DurationMetadata.ResidualCorrectionFactor = group.factor
	pred.DurationMetadata.ResidualCorrectionSource = group.source
	pred.DurationMetadata.Explanations = append(
		pred.DurationMetadata.Explanations,
		"recent local residual correction applied",
	)
	_ = jobID
}

func scalePrediction(pred *predictor.Prediction, factor float64) {
	if pred == nil || !(factor > 0) || math.IsNaN(factor) || math.IsInf(factor, 0) {
		return
	}
	pred.Mean *= factor
	pred.Lower *= factor
	pred.Upper *= factor
	pred.Std *= factor
}

func lookupResidualCorrection(project, gpuClass, command string) (residualGroup, bool) {
	database, err := db.OpenForReading()
	if err != nil {
		return residualGroup{}, false
	}
	defer database.Close()

	for _, spec := range []struct {
		source string
		where  string
		args   []any
	}{
		{
			source: "command+gpu",
			where:  "command = ? AND COALESCE(actual_gpu_class, requested_gpu_class, gpu_class, '') = ?",
			args:   []any{command, gpuClass},
		},
		{
			source: "project+gpu",
			where:  "project = ? AND COALESCE(actual_gpu_class, requested_gpu_class, gpu_class, '') = ?",
			args:   []any{project, gpuClass},
		},
		{
			source: "project",
			where:  "project = ?",
			args:   []any{project},
		},
	} {
		if group, ok := queryResidualGroup(database, spec.source, spec.where, spec.args...); ok {
			return group, true
		}
	}
	return residualGroup{}, false
}

func queryResidualGroup(database *sql.DB, source, where string, args ...any) (residualGroup, bool) {
	query := `
		SELECT duration_s,
		       json_extract(placement_meta, '$.pred_dur_s') AS pred_dur_s,
		       COALESCE(end_time, start_time, archived_at, 0) AS ts
		  FROM training_examples
		 WHERE duration_s > 5
		   AND json_extract(placement_meta, '$.pred_dur_s') > 0
		   AND LOWER(TRIM(COALESCE(status, ''))) IN ('', 'completed')
		   AND (exit_code IS NULL OR exit_code = 0)
		   AND TRIM(COALESCE(failure_reason, '')) = ''
		   AND ` + where + `
		 ORDER BY ts DESC
		 LIMIT 100`
	rows, err := database.Query(query, args...)
	if err != nil {
		return residualGroup{}, false
	}
	defer rows.Close()

	now := time.Now().Unix()
	var weighted, weightSum float64
	n := 0
	for rows.Next() {
		var actual, predicted float64
		var ts int64
		if err := rows.Scan(&actual, &predicted, &ts); err != nil {
			return residualGroup{}, false
		}
		if actual <= 0 || predicted <= 0 {
			continue
		}
		age := time.Duration(maxInt64(0, now-ts)) * time.Second
		weight := math.Pow(0.5, float64(age)/float64(residualHalfLife))
		weighted += weight * (actual / predicted)
		weightSum += weight
		n++
	}
	if n < residualMinSamples || weightSum <= 0 {
		return residualGroup{}, false
	}
	factor := weighted / weightSum
	if factor < 0.5 {
		factor = 0.5
	}
	if factor > 2.0 {
		factor = 2.0
	}
	return residualGroup{source: source, factor: factor, n: n}, true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
