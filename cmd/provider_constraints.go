package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/osteele/weft/internal/campaign"
	"github.com/spf13/cobra"
)

var providerConstraintsCmd = &cobra.Command{
	Use:   "constraints",
	Short: "Show how each provider honours each job constraint",
	Long: `Show, per provider, which job constraints are enforced and how.

The same job does not mean the same thing to every provider. RunPod sizes
container disk at creation rather than selecting on it, publishes no
reliability score, and exposes no driver version until the instance is
running. Weft does not paper over that — it would mean inventing values it
cannot observe — so this is where the differences are visible.

Statuses:
  weft re-checks      verified locally against returned offers
  provider filters    only the provider's own search enforces it
  not applicable      the provider has no such concept
  NOT ENFORCED        accepted from you and honoured by nobody`,
	RunE: runProviderConstraints,
}

func runProviderConstraints(cmd *cobra.Command, args []string) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tCONSTRAINT\tENFORCEMENT\tNOTES")
	for _, r := range campaign.ExplainAxisEnforcement() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Provider, r.Axis, r.Status, r.Detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var gaps int
	for _, r := range campaign.ExplainAxisEnforcement() {
		if r.Status == "NOT ENFORCED" {
			gaps++
		}
	}
	if gaps > 0 {
		fmt.Printf("\n%d constraint(s) are accepted but not honoured. A job setting one of these\n"+
			"on the affected provider will run without it.\n", gaps)
	}
	return nil
}
