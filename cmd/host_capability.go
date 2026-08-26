package cmd

import (
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostcap"
	"github.com/spf13/cobra"
)

var (
	hostCapabilityObserveSource string
	hostCapabilityObserveAbsent bool
	hostCapabilityObserveDetail string
)

var hostCapabilityCmd = &cobra.Command{
	Use:   "capability",
	Short: "Record advisory observations about host capabilities",
}

var hostCapabilityObserveCmd = &cobra.Command{
	Use:   "observe <host> <label>",
	Short: "Record an observer's advisory capability finding",
	Long: `Record whether an external observer found a capability on an inventory host.

Observations are reported by host list --json but never change placement eligibility.`,
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runHostCapabilityObserve,
}

func init() {
	hostCapabilityCmd.AddCommand(hostCapabilityObserveCmd)
	hostCapabilityObserveCmd.Flags().StringVar(&hostCapabilityObserveSource, "source", "", "Observation source in probe:<name> form (required)")
	hostCapabilityObserveCmd.Flags().BoolVar(&hostCapabilityObserveAbsent, "absent", false, "Record that the capability was not observed")
	hostCapabilityObserveCmd.Flags().StringVar(&hostCapabilityObserveDetail, "detail", "", "Opaque observation detail, provisional and capped at 256 bytes")
	_ = hostCapabilityObserveCmd.MarkFlagRequired("source")
}

func runHostCapabilityObserve(cmd *cobra.Command, args []string) error {
	host, label := args[0], args[1]
	if isInstanceIDArg(host) {
		return fmt.Errorf("weft host capability observe records inventory-host observations; cloud instances are ephemeral")
	}
	labels, err := hostcap.Normalize("", []string{label})
	if err != nil {
		return fmt.Errorf("normalize host capability: %w", err)
	}
	label = labels[0]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	observation := db.HostCapabilityObservation{
		Host:       host,
		Label:      label,
		Source:     hostCapabilityObserveSource,
		Observed:   !hostCapabilityObserveAbsent,
		Detail:     hostCapabilityObserveDetail,
		ObservedAt: time.Now(),
	}
	if err := db.RecordHostCapabilityObservation(database, observation); err != nil {
		return fmt.Errorf("observe host capability: %w", err)
	}

	state := "present"
	if !observation.Observed {
		state = "absent"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Recorded %s as %s on %s from %s; this observation does not affect placement.\n", label, state, host, hostCapabilityObserveSource)
	return nil
}
