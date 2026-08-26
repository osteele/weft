package db

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/osteele/weft/internal/hostcap"
)

// hostCapabilityObservationDetailMaxBytes is small on purpose: detail is a
// provisional field, kept open only until there is evidence of what observers
// record there. See HostCapabilityObservation in specs/inventory-placement.allium.
const hostCapabilityObservationDetailMaxBytes = 256

var hostCapabilityObservationSourcePattern = regexp.MustCompile(`^probe:[a-z0-9][a-z0-9._-]{0,63}$`)

// HostCapabilityObservation is a row in host_capability_observations: one
// observer's advisory finding about one capability label on one host.
type HostCapabilityObservation struct {
	Host       string
	Label      string
	Source     string
	Observed   bool
	Detail     string
	ObservedAt time.Time
}

// RecordHostCapabilityObservation records or replaces one observer's finding.
// Observations are advisory and do not alter placement; see
// HostCapabilityObservation in specs/inventory-placement.allium.
func RecordHostCapabilityObservation(database *sql.DB, observation HostCapabilityObservation) error {
	if strings.TrimSpace(observation.Host) == "" {
		return fmt.Errorf("host capability observation: host is required")
	}
	labels, err := hostcap.Normalize("", []string{observation.Label})
	if err != nil {
		return fmt.Errorf("host capability observation for %q: %w", observation.Host, err)
	}
	observation.Label = labels[0]
	if !hostCapabilityObservationSourcePattern.MatchString(observation.Source) {
		return fmt.Errorf("host capability observation for %q/%q: invalid source %q: want probe:<name>", observation.Host, observation.Label, observation.Source)
	}
	if len(observation.Detail) > hostCapabilityObservationDetailMaxBytes {
		return fmt.Errorf("host capability observation for %q/%q from %q: detail is %d bytes; maximum is %d", observation.Host, observation.Label, observation.Source, len(observation.Detail), hostCapabilityObservationDetailMaxBytes)
	}
	if observation.ObservedAt.IsZero() {
		return fmt.Errorf("host capability observation for %q/%q from %q: observed_at is required", observation.Host, observation.Label, observation.Source)
	}

	_, err = database.Exec(`
		INSERT INTO host_capability_observations (host, label, source, observed, detail, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(host, label, source) DO UPDATE SET
			observed = excluded.observed,
			detail = excluded.detail,
			observed_at = excluded.observed_at
	`, observation.Host, observation.Label, observation.Source, observation.Observed, observation.Detail, observation.ObservedAt.Unix())
	if err != nil {
		return fmt.Errorf("record host capability observation for %q/%q from %q: %w", observation.Host, observation.Label, observation.Source, err)
	}
	return nil
}

// ListHostCapabilityObservations lists one host's observations by label and
// source.
func ListHostCapabilityObservations(database *sql.DB, host string) ([]HostCapabilityObservation, error) {
	rows, err := database.Query(`
		SELECT host, label, source, observed, detail, observed_at
		FROM host_capability_observations
		WHERE host = ?
		ORDER BY label, source
	`, host)
	if err != nil {
		return nil, fmt.Errorf("list host capability observations for %q: %w", host, err)
	}
	defer rows.Close()

	observations, err := scanHostCapabilityObservations(rows)
	if err != nil {
		return nil, fmt.Errorf("list host capability observations for %q: %w", host, err)
	}
	return observations, nil
}

// ListAllHostCapabilityObservations groups all observations by host. Each
// host's slice is ordered by label and source.
func ListAllHostCapabilityObservations(database *sql.DB) (map[string][]HostCapabilityObservation, error) {
	rows, err := database.Query(`
		SELECT host, label, source, observed, detail, observed_at
		FROM host_capability_observations
		ORDER BY host, label, source
	`)
	if err != nil {
		return nil, fmt.Errorf("list all host capability observations: %w", err)
	}
	defer rows.Close()

	observations, err := scanHostCapabilityObservations(rows)
	if err != nil {
		return nil, fmt.Errorf("list all host capability observations: %w", err)
	}
	byHost := make(map[string][]HostCapabilityObservation)
	for _, observation := range observations {
		byHost[observation.Host] = append(byHost[observation.Host], observation)
	}
	return byHost, nil
}

func scanHostCapabilityObservations(rows *sql.Rows) ([]HostCapabilityObservation, error) {
	var observations []HostCapabilityObservation
	for rows.Next() {
		var (
			observation HostCapabilityObservation
			detail      sql.NullString
			observedAt  int64
		)
		if err := rows.Scan(
			&observation.Host,
			&observation.Label,
			&observation.Source,
			&observation.Observed,
			&detail,
			&observedAt,
		); err != nil {
			return nil, fmt.Errorf("scan host capability observation: %w", err)
		}
		if detail.Valid {
			observation.Detail = detail.String
		}
		observation.ObservedAt = time.Unix(observedAt, 0)
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate host capability observations: %w", err)
	}
	return observations, nil
}
