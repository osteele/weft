package db

import (
	"database/sql"
	"fmt"
	"time"
)

// HostAgentState is the latest independently observed deployment and runtime
// identity for one inventory host. Zero timestamps mean that source has never
// produced an observation.
type HostAgentState struct {
	Host                 string
	DeployedVersion      string
	DeployedObservedAt   int64
	RunningVersion       string
	QueueProtocolVersion int
	RunningObservedAt    int64
}

// RecordHostAgentDeployment records a successfully verified installed binary.
// A failed or unknown probe must not call this function, because it must not
// erase the last positive observation.
func RecordHostAgentDeployment(database *sql.DB, host, version string, observedAt time.Time) error {
	if host == "" || version == "" {
		return fmt.Errorf("host and deployed agent version are required")
	}
	_, err := database.Exec(`
		INSERT INTO host_agent_state (host, deployed_version, deployed_observed_at)
		VALUES (?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET
			deployed_version=excluded.deployed_version,
			deployed_observed_at=excluded.deployed_observed_at`,
		host, version, observedAt.Unix())
	return err
}

// RecordHostAgentRuntime records identity published by a runner state snapshot.
// Empty version and protocol zero are valid positive observations of a legacy
// runner that does not publish those fields.
func RecordHostAgentRuntime(database *sql.DB, host, version string, protocolVersion int, observedAt time.Time) error {
	if host == "" {
		return fmt.Errorf("host is required")
	}
	_, err := database.Exec(`
		INSERT INTO host_agent_state (host, running_version, queue_protocol_version, running_observed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET
			running_version=excluded.running_version,
			queue_protocol_version=excluded.queue_protocol_version,
			running_observed_at=excluded.running_observed_at`,
		host, version, protocolVersion, observedAt.Unix())
	return err
}

// ListHostAgentStates returns the cached inventory-agent observations ordered
// by host. It performs no network access.
func ListHostAgentStates(database *sql.DB) ([]HostAgentState, error) {
	rows, err := database.Query(`
		SELECT host, deployed_version, deployed_observed_at,
		       running_version, queue_protocol_version, running_observed_at
		FROM host_agent_state
		ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []HostAgentState
	for rows.Next() {
		var state HostAgentState
		var deployedVersion, runningVersion sql.NullString
		var deployedAt, runningAt, protocol sql.NullInt64
		if err := rows.Scan(
			&state.Host, &deployedVersion, &deployedAt,
			&runningVersion, &protocol, &runningAt,
		); err != nil {
			return nil, err
		}
		state.DeployedVersion = deployedVersion.String
		state.DeployedObservedAt = deployedAt.Int64
		state.RunningVersion = runningVersion.String
		state.QueueProtocolVersion = int(protocol.Int64)
		state.RunningObservedAt = runningAt.Int64
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return states, nil
}
