package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/spf13/cobra"
)

const (
	hostAgentStatusSchemaVersion = 2
	// Observations are recorded by the daemon's host-sync passes. During
	// quiet periods those passes run on daemonQuietSyncInterval (5m), so a
	// healthy observation can legitimately lag several minutes; a bound
	// tighter than the recording cadence renders normal hosts stale around
	// the clock (wb162). Twice the quiet interval absorbs one skipped pass
	// on both the publish and sync sides.
	hostAgentRuntimeFreshness = 10 * time.Minute
)

var hostAgentStatusJSON bool

var hostAgentStatusCmd = &cobra.Command{
	Use:   "agent-status",
	Short: "Show cached agent versions across inventory hosts",
	Long: `Show the last verified deployed agent and the latest runner identity for
every inventory host. This command reads the local cache and performs no SSH or
R2 requests. Observation ages identify facts that may no longer be current.`,
	Args: cobra.NoArgs,
	RunE: runHostAgentStatus,
}

func init() {
	hostCmd.AddCommand(hostAgentStatusCmd)
	hostAgentStatusCmd.Flags().BoolVar(&hostAgentStatusJSON, "json", false, "Print versioned JSON")
}

type hostAgentStatusRow struct {
	Host                 string `json:"host"`
	Status               string `json:"status"`
	Reason               string `json:"reason"`
	DesiredVersion       string `json:"desired_version"`
	DesiredError         string `json:"desired_error,omitempty"`
	DeployedVersion      string `json:"deployed_version"`
	DeployedObservedAt   int64  `json:"deployed_observed_at"`
	RunningVersion       string `json:"running_version"`
	QueueProtocolVersion int    `json:"queue_protocol_version"`
	RunningObservedAt    int64  `json:"running_observed_at"`
}

type hostAgentStatusDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedAt   int64                `json:"generated_at"`
	Hosts         []hostAgentStatusRow `json:"hosts"`
}

func runHostAgentStatus(cmd *cobra.Command, _ []string) error {
	hosts, err := inventory.LoadHosts()
	if err != nil {
		return fmt.Errorf("load inventory hosts: %w", err)
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	observations, err := db.ListHostAgentStates(database)
	if err != nil {
		return fmt.Errorf("list host agent observations: %w", err)
	}

	now := time.Now()
	rows := buildHostAgentStatusRows(hosts, observations, agentdeploy.LocalAgentVersionForTarget, now)
	if hostAgentStatusJSON {
		return writeHostAgentStatusJSON(cmd.OutOrStdout(), rows, now)
	}
	return writeHostAgentStatusTable(cmd.OutOrStdout(), rows, now)
}

func buildHostAgentStatusRows(
	hosts []inventory.HostSpec,
	observations []db.HostAgentState,
	resolveDesired func(goos, goarch string) (string, error),
	now time.Time,
) []hostAgentStatusRow {
	byHost := make(map[string]db.HostAgentState, len(observations))
	for _, observation := range observations {
		byHost[observation.Host] = observation
	}
	type targetKey struct {
		goos   string
		goarch string
	}
	type desiredResult struct {
		version string
		err     error
	}
	desiredByTarget := make(map[targetKey]desiredResult)
	rows := make([]hostAgentStatusRow, 0, len(hosts))
	for _, host := range hosts {
		observation := byHost[host.Name]
		key := targetKey{goos: host.OS, goarch: host.Arch}
		desired, ok := desiredByTarget[key]
		if !ok {
			desired.version, desired.err = resolveDesired(host.OS, host.Arch)
			desiredByTarget[key] = desired
		}
		row := hostAgentStatusRow{
			Host:                 host.Name,
			DesiredVersion:       desired.version,
			DeployedVersion:      observation.DeployedVersion,
			DeployedObservedAt:   observation.DeployedObservedAt,
			RunningVersion:       observation.RunningVersion,
			QueueProtocolVersion: observation.QueueProtocolVersion,
			RunningObservedAt:    observation.RunningObservedAt,
		}
		if desired.err != nil {
			row.DesiredError = desired.err.Error()
		}
		row.Status, row.Reason = classifyHostAgentStatus(row, now)
		rows = append(rows, row)
	}
	return rows
}

func classifyHostAgentStatus(row hostAgentStatusRow, now time.Time) (string, string) {
	if row.RunningObservedAt == 0 {
		return "unknown", "runner identity has not been observed"
	}
	runningAge := now.Sub(time.Unix(row.RunningObservedAt, 0))
	if runningAge < 0 {
		return "stale-observation", "runner observation timestamp is in the future"
	}
	if runningAge > hostAgentRuntimeFreshness {
		return "stale-observation", fmt.Sprintf("runner identity was observed %s ago", formatAgentObservationAge(runningAge))
	}
	if row.RunningVersion == "" {
		return "stale", "runner does not publish an agent version"
	}
	if row.QueueProtocolVersion < opsqueue.QueueProtocolVersion {
		return "stale", fmt.Sprintf("runner protocol %d is older than required version %d", row.QueueProtocolVersion, opsqueue.QueueProtocolVersion)
	}
	if row.DesiredError != "" {
		return "unknown", "desired agent version unavailable: " + row.DesiredError
	}
	if row.DesiredVersion == "" {
		return "unknown", "desired agent version is unavailable"
	}
	// A revision fallback names the tree, not its bytes, so it disagrees with
	// a source hash of the same tree. Reporting that as staleness would accuse
	// a current host of running an old build because this process could not
	// run `go list`.
	if agentdeploy.AgentVersionKindOf(row.DesiredVersion) == agentdeploy.AgentVersionRevisionFallback &&
		(agentdeploy.AgentVersionKindOf(row.RunningVersion) == agentdeploy.AgentVersionSourceHash ||
			agentdeploy.AgentVersionKindOf(row.DeployedVersion) == agentdeploy.AgentVersionSourceHash) {
		return "unknown", "desired agent version is a revision fallback and cannot be compared with a source-hash build"
	}
	if row.RunningVersion != row.DesiredVersion {
		return "stale", "running agent differs from the desired build"
	}
	if row.DeployedVersion == "" {
		return "unknown", "deployed binary has not been verified"
	}
	if row.DeployedVersion != row.DesiredVersion {
		return "stale", "deployed binary differs from the desired build"
	}
	return "current", "desired, deployed, and running versions agree"
}

func writeHostAgentStatusJSON(w io.Writer, rows []hostAgentStatusRow, now time.Time) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(hostAgentStatusDocument{
		SchemaVersion: hostAgentStatusSchemaVersion,
		GeneratedAt:   now.Unix(),
		Hosts:         rows,
	})
}

func writeHostAgentStatusTable(w io.Writer, rows []hostAgentStatusRow, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSTATUS\tDESIRED\tDEPLOYED\tDEPLOYED AGE\tRUNNING\tPROTOCOL\tRUNNING AGE\tDETAIL")
	for _, row := range rows {
		protocol := "-"
		if row.RunningObservedAt > 0 {
			protocol = fmt.Sprintf("%d", row.QueueProtocolVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Host, row.Status, row.DesiredVersion,
			versionOrUnknown(row.DeployedVersion), agentObservationAge(row.DeployedObservedAt, now),
			versionOrUnknown(row.RunningVersion), protocol,
			agentObservationAge(row.RunningObservedAt, now), row.Reason)
	}
	return tw.Flush()
}

func versionOrUnknown(version string) string {
	if version == "" {
		return "unknown"
	}
	return version
}

func agentObservationAge(observedAt int64, now time.Time) string {
	if observedAt == 0 {
		return "never"
	}
	return formatAgentObservationAge(now.Sub(time.Unix(observedAt, 0)))
}

func formatAgentObservationAge(age time.Duration) string {
	if age < 0 {
		return "in the future"
	}
	return age.Round(time.Second).String()
}
