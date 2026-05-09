package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/blackboard"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(blackboardCmd)
	blackboardCmd.AddCommand(blackboardStatusCmd)
	blackboardStatusCmd.Flags().Bool("json", false, "Print machine-readable JSON")
}

var blackboardCmd = &cobra.Command{
	Use:   "blackboard",
	Short: "Inspect the federated R2 blackboard",
}

var blackboardStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show federated R2 blackboard state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newR2Client()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), r2CmdTimeout)
		defer cancel()
		status, err := blackboard.FetchStatus(ctx, client, time.Now())
		if err != nil {
			return err
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(status)
		}
		printBlackboardStatus(status)
		return nil
	},
}

func printBlackboardStatus(status *blackboard.Status) {
	if status == nil {
		fmt.Println("Blackboard: unavailable")
		return
	}
	if status.Autopilot == nil {
		fmt.Println("Autopilot: not exported")
	} else {
		parts := []string{status.Autopilot.State}
		if status.Autopilot.Runner != "" {
			parts = append(parts, "runner="+status.Autopilot.Runner)
		}
		if status.Autopilot.HeartbeatAt != "" {
			parts = append(parts, "heartbeat="+status.Autopilot.HeartbeatAt)
		}
		fmt.Println("Autopilot:", strings.Join(parts, " "))
	}
	fmt.Printf("Jobs: %d specs, %d claims (%d active, %d expired), %d assignments\n",
		status.Specs, status.Claims, status.ActiveClaims, status.ExpiredClaims, status.Assignments)
	fmt.Printf("Agents: %d heartbeats\n", status.AgentHeartbeats)
	fmt.Printf("Events: %d\n", status.Events)
	for _, err := range status.ClaimErrors {
		fmt.Printf("Warning: %s\n", err)
	}
}
