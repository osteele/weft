// Package llmusage records every LLM API call weft makes to a billable
// provider, and exposes aggregations for the dashboard's Usage and Cost
// views.
//
// Recording is best-effort: callers should never let a usage-recording
// failure bubble up and kill an LLM call. The package's public functions
// swallow DB errors and return only structured aggregates.
package llmusage

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// Provider identifies the upstream LLM provider. Keep these stable strings
// — they're stored verbatim in the llm_calls table and queried by views.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
)

// Feature is the in-product use-case the call serves. Keep stable.
const (
	FeatureNarrate  = "narrate"
	FeatureCompact  = "compact"
	FeatureDescribe = "describe"
)

// Call is a single LLM API invocation to be recorded.
type Call struct {
	When                time.Time
	Provider            string // one of the Provider* constants
	Model               string // exact model id sent to the provider
	Feature             string // one of the Feature* constants
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	Latency             time.Duration
	JobID               *int64 // optional: associated weft job
	Error               string // empty on success; non-empty if the call failed but we want to record the attempt
}

// CostMicros computes the cost in micro-dollars for this call using the
// Anthropic pricing table. Returns 0 for unknown models.
func (c Call) CostMicros() int64 {
	return CostMicros(PriceForModel(c.Model), c.InputTokens, c.OutputTokens, c.CacheCreationTokens, c.CacheReadTokens)
}

// Recorder writes Call records to the database. Use NewRecorder to construct.
//
// Methods are safe for concurrent use.
type Recorder struct {
	db *sql.DB
	mu sync.Mutex
}

// NewRecorder returns a Recorder backed by the given DB handle. Callers may
// pass nil to get a no-op recorder (handy for tests).
func NewRecorder(database *sql.DB) *Recorder {
	return &Recorder{db: database}
}

// Record persists one Call. Errors are returned but production callers
// should typically ignore them — the only purpose of recording is observability
// and a missed row is better than a failed user-facing call.
func (r *Recorder) Record(c Call) error {
	if r == nil || r.db == nil {
		return nil
	}
	if c.When.IsZero() {
		c.When = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`
		INSERT INTO llm_calls (
			ts_unix, provider, model, feature,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			latency_ms, cost_micros, job_id, error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		c.When.Unix(), c.Provider, c.Model, c.Feature,
		c.InputTokens, c.OutputTokens, c.CacheCreationTokens, c.CacheReadTokens,
		c.Latency.Milliseconds(), c.CostMicros(), c.JobID, c.Error,
	)
	if err != nil {
		return fmt.Errorf("insert llm_calls: %w", err)
	}
	return nil
}

// FeatureUsage aggregates calls in a given window by (feature, model).
type FeatureUsage struct {
	Feature             string
	Model               string
	Calls               int
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	CostMicros          int64
	LastCallAt          time.Time
}

// USD returns the cost as a USD float for display.
func (u FeatureUsage) USD() float64 { return float64(u.CostMicros) / 1_000_000.0 }

// ListFeatureUsageSince returns per-(feature, model) aggregates for calls
// recorded since sinceUnix. Rows are sorted by cost descending.
func ListFeatureUsageSince(database *sql.DB, sinceUnix int64) ([]FeatureUsage, error) {
	if database == nil {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT feature, model,
		       COUNT(*) AS calls,
		       COALESCE(SUM(input_tokens),0)          AS in_tok,
		       COALESCE(SUM(output_tokens),0)         AS out_tok,
		       COALESCE(SUM(cache_creation_tokens),0) AS cw_tok,
		       COALESCE(SUM(cache_read_tokens),0)     AS cr_tok,
		       COALESCE(SUM(cost_micros),0)           AS cost_micros,
		       MAX(ts_unix)                            AS last_ts
		FROM llm_calls
		WHERE ts_unix >= ?
		GROUP BY feature, model
		ORDER BY cost_micros DESC, calls DESC
	`, sinceUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureUsage
	for rows.Next() {
		var u FeatureUsage
		var lastTS int64
		if err := rows.Scan(&u.Feature, &u.Model, &u.Calls,
			&u.InputTokens, &u.OutputTokens, &u.CacheCreationTokens, &u.CacheReadTokens,
			&u.CostMicros, &lastTS); err != nil {
			return nil, err
		}
		u.LastCallAt = time.Unix(lastTS, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// DailySpend totals cost (USD) and call count for the last n days (newest
// first). Returns exactly n entries, padding with zeros for days with no
// activity, so the result is suitable for a sparkline.
type DaySpend struct {
	Date       time.Time // local-midnight start of the day
	CostMicros int64
	Calls      int
}

// DailySpendLastN returns per-day spend for the last n days, newest first.
func DailySpendLastN(database *sql.DB, n int) ([]DaySpend, error) {
	if database == nil || n <= 0 {
		return nil, nil
	}
	now := time.Now()
	out := make([]DaySpend, n)
	for i := 0; i < n; i++ {
		out[i].Date = startOfDay(now.AddDate(0, 0, -i))
	}
	cutoff := startOfDay(now.AddDate(0, 0, -(n - 1)))
	rows, err := database.Query(`
		SELECT ts_unix, cost_micros FROM llm_calls WHERE ts_unix >= ?
	`, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts, cost int64
		if err := rows.Scan(&ts, &cost); err != nil {
			return nil, err
		}
		day := startOfDay(time.Unix(ts, 0))
		// Find slot.
		offset := int(startOfDay(now).Sub(day) / (24 * time.Hour))
		if offset < 0 || offset >= n {
			continue
		}
		out[offset].CostMicros += cost
		out[offset].Calls++
	}
	return out, rows.Err()
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
