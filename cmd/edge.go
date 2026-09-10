package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/appdirs"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/secrets"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(edgeCmd)
	edgeCmd.AddCommand(edgeDoctorCmd, edgeWaitCmd, edgeKeyCmd)
	edgeKeyCmd.AddCommand(edgeKeyMintCmd, edgeKeyAddCmd, edgeKeyListCmd,
		edgeKeyRevokeCmd, edgeKeyRenewCmd)

	edgeDoctorCmd.Flags().String("transport", "", "Override transport: a directory path uses the filesystem transport")
	edgeWaitCmd.Flags().Duration("timeout", 30*time.Minute, "How long to wait before returning")
	edgeWaitCmd.Flags().String("class", "job", "Expectation class for the statistics envelope")
	edgeKeyMintCmd.Flags().String("plan", "", "Plan execution this key is minted for (required)")
	edgeKeyAddCmd.Flags().String("plan", "", "Plan execution this key is for")
	edgeKeyAddCmd.Flags().String("host", "", "Host this key is bound to (required)")
	edgeKeyAddCmd.Flags().String("public-key", "", "Base64 ed25519 public key (required)")
	edgeKeyAddCmd.Flags().Float64("spend-ceiling", 0, "Spend granted to this plan, in USD")
	edgeKeyAddCmd.Flags().String("project", "", "Project whose settled remote ownership gates renewal")
	edgeKeyListCmd.Flags().Bool("json", false, "Print machine-readable JSON")
	edgeKeyRevokeCmd.Flags().Bool("compromised", false,
		"Repudiate the key's past signatures as well as refusing new ones")
}

var edgeCmd = &cobra.Command{
	Use:   "edge",
	Short: "Submit work to a hub from an edge host, and manage edge keys",
	Long: "Edge submission lets a host with no inbound route to the hub submit work\n" +
		"by writing objects to a store the hub pulls from.\n\n" +
		"See docs/design/edge-submission-protocol.md.",
}

// edgeRuntime assembles the runtime for this installation's configured role.
func edgeRuntime(transportOverride string) (*edge.Runtime, *config.Config, error) {
	return edgeRuntimeOpts(transportOverride, false)
}

// edgeRuntimeForDiagnostics assembles the runtime even when this edge has no
// signing key yet, so `doctor` can report that rather than fail on it.
func edgeRuntimeForDiagnostics(transportOverride string) (*edge.Runtime, *config.Config, error) {
	return edgeRuntimeOpts(transportOverride, true)
}

func edgeRuntimeOpts(transportOverride string, allowMissingSigner bool) (*edge.Runtime, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	transport, err := edgeTransport(cfg, transportOverride)
	if err != nil {
		return nil, nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, nil, fmt.Errorf(
			"cannot determine this host's name, which the edge policy needs to keep "+
				"submitted work off the hub: %w", err)
	}
	rt, err := edge.NewRuntime(transport, edge.RuntimeConfig{
		Role:                    cfg.Edge.Role,
		SigningKeyPath:          edgePlanKeyPath(cfg, cfg.Edge.PlanID),
		PlanID:                  cfg.Edge.PlanID,
		SubmitterHost:           cfg.Edge.SubmitterHost,
		KeyringDir:              edgeKeyringDir(cfg),
		SeenDir:                 edgeSeenDir(cfg),
		AllowedTargets:          cfg.Edge.AllowedTargets,
		MaxSpendUSD:             cfg.Edge.MaxSpendUSD,
		AllowForeignJobControl:  cfg.Edge.AllowForeignJobControl,
		HubHost:                 hostname,
		LeaseWindowHours:        cfg.Edge.LeaseWindowHours,
		LeaseRenewIntervalHours: cfg.Edge.LeaseRenewIntervalHours,
		AllowMissingSigner:      allowMissingSigner,
	})
	if err != nil {
		return nil, nil, err
	}
	return rt, cfg, nil
}

// edgeTransport builds the object store for the submission channel.
func edgeTransport(cfg *config.Config, override string) (edge.Transport, error) {
	return edgeStoreTransport("inbound", cfg.Edge.Inbound, override)
}

// edgeViewTransport builds the object store for the hub view.
func edgeViewTransport(cfg *config.Config, override string) (edge.Transport, error) {
	return edgeStoreTransport("view", cfg.Edge.View.R2(), override)
}

// edgeStoreTransport builds an object store for one edge channel. A directory
// override selects the filesystem transport, which runs the identical protocol
// with verification fully enabled — it is a real transport, not a bypass.
// Inbound and view share this so the two channels cannot drift apart in how a
// store is chosen.
func edgeStoreTransport(purpose string, cfg config.R2Config, override string) (edge.Transport, error) {
	if override != "" {
		return edge.NewFSTransport(override)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf(
			"no edge %s bucket configured.\n"+
				"Set [edge.%s] in ~/.config/weft/config.toml, or pass --transport <dir> "+
				"to use the filesystem transport.\n"+
				"The %s bucket is deliberately separate from the results bucket; see decision 0026.",
			purpose, purpose, purpose)
	}
	// Each channel's bucket is a separate store from results, so it gets its
	// own client rather than reusing the results one.
	client, err := r2.New(r2.Config{
		AccountID:       cfg.AccountID,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		Bucket:          cfg.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("build %s R2 client: %w", purpose, err)
	}
	return edge.NewR2Transport(client)
}

func edgeKeyringDir(cfg *config.Config) string {
	if cfg.Edge.KeyringDir != "" {
		return cfg.Edge.KeyringDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "edge-keyring"
	}
	return filepath.Join(home, ".config", "weft", "edge-keyring")
}

func edgeSeenDir(cfg *config.Config) string {
	if cfg.Edge.SeenDir != "" {
		return cfg.Edge.SeenDir
	}
	dir, err := appdirs.StateDir()
	if err != nil {
		return "edge-seen"
	}
	return filepath.Join(dir, "edge-seen")
}

// edgeFetchAck reads the hub's acknowledgement for a submission.
//
// A missing ack is ErrNotFound, which callers must treat as unknown rather
// than as a verdict: the hub may simply not have polled yet.
func edgeFetchAck(ctx context.Context, rt *edge.Runtime, nonce string) (*edge.Ack, error) {
	data, err := rt.Transport.Get(ctx, edge.AckKey(nonce))
	if err != nil {
		return nil, err
	}
	var ack edge.Ack
	if err := json.Unmarshal(data, &ack); err != nil {
		return nil, fmt.Errorf("decode acknowledgement for %s: %w", nonce, err)
	}
	return &ack, nil
}

var edgeDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the edge submission path end to end",
	Long: "Verifies configuration, reaches the inbound store, and round-trips a marker\n" +
		"object through it, reporting per-hop latency.\n\n" +
		"This is the health check, the stall diagnostic, and the way to tell whether a\n" +
		"stuck submission is the store, the hub, or the plan.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		override, _ := cmd.Flags().GetString("transport")
		rt, cfg, err := edgeRuntimeForDiagnostics(override)
		if err != nil {
			if loaded, loadErr := config.Load(); loadErr == nil {
				role := loaded.Edge.Role
				if role == "" {
					role = "hub"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Role:      %s\n", role)
				fmt.Fprintln(cmd.OutOrStdout(), "Transport: unavailable")
				printEdgeViewDoctor(cmd, loaded, role, override)
				printEdgeInboxDoctor(cmd, loaded, role)
			}
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()

		fmt.Printf("Role:      %s\n", rt.Role)
		fmt.Printf("Transport: %s\n", rt.Transport.Name())
		printEdgeViewDoctor(cmd, cfg, rt.Role, override)

		if rt.Role == "edge" {
			switch {
			case rt.Signer != nil:
				fmt.Printf("Signing:   key %s as host %s\n", rt.Signer.KeyID(), rt.SubmitterHost)
			default:
				fmt.Printf("Signing:   no key yet for host %s\n", rt.SubmitterHost)
				fmt.Println("           mint one with `weft edge key mint --plan <plan-id>`,")
				fmt.Println("           then register its public half on the hub with `weft edge key add`")
			}
		} else {
			printKeyringProblems(rt.Keyring)
			keys := rt.Keyring.List()
			fmt.Printf("Keyring:   %d key(s)\n", len(keys))
			now := time.Now()
			lapsed, soon := 0, 0
			for _, k := range keys {
				fmt.Printf("           %s\n", describeKey(k, now))
				if k.RevokedAt != nil || k.NotAfter.IsZero() {
					continue
				}
				switch {
				case now.After(k.NotAfter):
					lapsed++
				case k.NotAfter.Sub(now) < nearLapse:
					soon++
				}
			}
			if lapsed > 0 || soon > 0 {
				fmt.Printf("           %d lapsed, %d lapsing within %s\n",
					lapsed, soon, nearLapse)
			}
			spend := fmt.Sprintf("$%.2f", rt.Policy.MaxSpendUSD)
			if rt.Policy.MaxSpendUSD == 0 {
				spend = "unset (no spending authorized)"
			}
			fmt.Printf("Policy:    targets %v, max spend %s\n", rt.Policy.AllowedTargets, spend)
			fmt.Printf("Lease:     %s window renewed every %s while project ownership remains settled and remote\n",
				rt.Lease.Window, rt.Lease.Interval)
			if len(keys) == 0 {
				fmt.Println("\nNo keys registered. Mint one on the edge and register it here:")
				fmt.Println("  ssh agent@<edge> 'weft edge key mint --plan <plan-id>'")
				fmt.Println("  weft edge key add --host <edge> --plan <plan-id> --project <project> --public-key <key>")
			}
		}

		fmt.Println()
		marker := fmt.Sprintf("%sdoctor/%d", edge.PrefixAck, time.Now().UnixNano())
		body := []byte(time.Now().UTC().Format(time.RFC3339Nano))

		hops := []struct {
			name string
			fn   func() error
		}{
			{"write", func() error { return rt.Transport.Put(ctx, marker, body) }},
			{"read", func() error {
				got, err := rt.Transport.Get(ctx, marker)
				if err != nil {
					return err
				}
				if string(got) != string(body) {
					return fmt.Errorf("read back %q, wrote %q", got, body)
				}
				return nil
			}},
			{"list", func() error {
				_, err := rt.Transport.List(ctx, edge.PrefixAck)
				return err
			}},
			{"delete", func() error { return rt.Transport.Delete(ctx, marker) }},
		}

		total := time.Duration(0)
		for _, hop := range hops {
			start := time.Now()
			err := hop.fn()
			took := time.Since(start)
			total += took
			status := "ok"
			if err != nil {
				status = "FAILED"
			}
			fmt.Printf("  %-7s %-7s %s\n", hop.name, status, took.Round(time.Millisecond))
			if err != nil {
				fmt.Printf("\nRound trip failed at the %s hop: %v\n", hop.name, err)
				return fmt.Errorf("edge doctor: %s hop failed", hop.name)
			}
		}
		fmt.Printf("\nRound trip ok in %s.\n", total.Round(time.Millisecond))
		printEdgeInboxDoctor(cmd, cfg, rt.Role)
		return nil
	},
}

func printEdgeInboxDoctor(cmd *cobra.Command, cfg *config.Config, role string) {
	if role == "edge" {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout())
	if cfg == nil || cfg.Edge.Inbound.Bucket == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Inbox:     poller not configured")
		return
	}
	state, err := loadEdgeInboxState()
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Inbox:     poller state unreadable: %v\n", err)
		return
	}
	if state.LastSuccessfulPoll.IsZero() {
		fmt.Fprintln(cmd.OutOrStdout(), "Inbox:     no successful poll recorded")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Inbox:     last polled %s ago (%s), %d pointer(s) pending\n",
			edgeview.FormatAge(time.Since(state.LastSuccessfulPoll)),
			util.FormatCLITime(state.LastSuccessfulPoll, "2006-01-02 15:04:05"), state.PendingPointers)
	}
	if state.PollError != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Inbox error: %s\n", state.PollError)
	}
}

func printEdgeViewDoctor(cmd *cobra.Command, cfg *config.Config, role, override string) {
	fmt.Fprintln(cmd.OutOrStdout())
	if role != "edge" {
		if cfg == nil || !cfg.Edge.View.Configured() {
			fmt.Fprintln(cmd.OutOrStdout(), "View:      not configured")
			return
		}
		state, err := loadEdgeViewPublishState()
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "View:      publisher state unreadable: %v\n", err)
			return
		}
		if state.LastSuccessfulPublish.IsZero() {
			fmt.Fprintln(cmd.OutOrStdout(), "View:      no successful publish recorded")
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "View:      last published %s ago (%s)\n",
				edgeview.FormatAge(time.Since(state.LastSuccessfulPublish)), util.FormatCLITime(state.LastSuccessfulPublish, "2006-01-02 15:04:05"))
		}
		if state.PublishError != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "View error: %s\n", state.PublishError)
		}
		for _, name := range sortedStringMapKeys(state.SectionErrors) {
			fmt.Fprintf(cmd.OutOrStdout(), "View section %s: FAILED: %s\n", name, state.SectionErrors[name])
		}
		return
	}

	transport, err := edgeViewTransport(cfg, override)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "View:      unavailable: %v\n", err)
		return
	}
	manifest, err := edgeview.NewReader(transport).Manifest(cmd.Context())
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "View:      unavailable: %s\n", edgeViewCause(err))
		return
	}
	now := time.Now()
	bound := cfg.Edge.View.StaleAfter()
	manifestAge := now.Sub(manifest.PublishedAt)
	fmt.Fprintf(cmd.OutOrStdout(), "View manifest: present, age %s (bound %s, %s)\n",
		edgeview.FormatAge(manifestAge), edgeview.FormatAge(bound), freshnessLabel(manifestAge, bound))
	for _, name := range edgeViewSectionNames(manifest) {
		age := now.Sub(manifest.Sections[name].PublishedAt)
		fmt.Fprintf(cmd.OutOrStdout(), "View section %s: age %s (bound %s, %s)\n",
			name, edgeview.FormatAge(age), edgeview.FormatAge(bound), freshnessLabel(age, bound))
	}
	for _, name := range sortedStringMapKeys(manifest.Errors) {
		fmt.Fprintf(cmd.OutOrStdout(), "View section %s: FAILED: %s\n", name, manifest.Errors[name])
	}
}

func freshnessLabel(age, bound time.Duration) string {
	if bound > 0 && age > bound {
		return "stale"
	}
	return "fresh"
}

func sortedStringMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// nearLapse is how close to expiry a key is worth calling out. A near lapse
// indicates that renewal is deferred or failing and needs investigation.
const nearLapse = 2 * time.Hour

func describeKey(k edge.Key, now time.Time) string {
	state := "active"
	switch {
	case k.RevokedAt != nil:
		state = "REVOKED " + util.FormatCLITime(*k.RevokedAt, "2006-01-02 15:04:05")
	case !k.NotAfter.IsZero() && now.After(k.NotAfter):
		state = "LAPSED " + util.FormatCLITime(k.NotAfter, "2006-01-02 15:04:05")
	case !k.NotAfter.IsZero() && k.NotAfter.Sub(now) < nearLapse:
		state = fmt.Sprintf("lapses in %s (%s)",
			k.NotAfter.Sub(now).Round(time.Minute), util.FormatCLITime(k.NotAfter, "2006-01-02 15:04:05"))
	case !k.NotAfter.IsZero():
		state = "valid until " + util.FormatCLITime(k.NotAfter, "2006-01-02 15:04:05")
	}
	plan := k.PlanID
	if plan == "" {
		plan = "(no plan)"
	}
	// Zero is enforced as "permits nothing". Rendering it as "no ceiling"
	// would read as unlimited — the display contradicting the enforcement.
	ceiling := "no spend authorized"
	if k.SpendCeilingUSD > 0 {
		ceiling = fmt.Sprintf("$%.2f", k.SpendCeilingUSD)
	}
	return fmt.Sprintf("%-24s host=%-10s plan=%-16s %-40s %s",
		k.KeyID, k.Host, plan, state, ceiling)
}

var edgeWaitCmd = &cobra.Command{
	Use:   "wait <nonce>",
	Short: "Wait for the hub to acknowledge a submission",
	Long: "Blocks in one call rather than returning for repeated polling.\n\n" +
		"Progress reports carry elapsed time, the current phase, statistics with their\n" +
		"provenance, an explicit normality verdict, and the trigger that would change it.\n" +
		"A missing acknowledgement is reported as unknown, never as failure.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nonce := args[0]
		timeout, _ := cmd.Flags().GetDuration("timeout")
		class, _ := cmd.Flags().GetString("class")

		rt, cfg, err := edgeRuntime("")
		if err != nil {
			return err
		}
		expect := edgeExpectation(cfg, class)

		// Measure from the submission, not from this process. A wait that
		// times out and is resumed would otherwise restart its clock, so an
		// escalation ceiling measured in hours could never be reached by any
		// single invocation.
		start, err := edge.NonceTime(nonce)
		if err != nil {
			return fmt.Errorf("cannot read the submission time from nonce %s: %w", nonce, err)
		}
		if expect.Escalate > 0 && timeout < expect.Escalate {
			fmt.Printf("Note: --timeout %s is shorter than the %s escalation ceiling, so this "+
				"invocation will time out before it can escalate. Elapsed is measured from the "+
				"submission, so resuming continues to accumulate toward it.\n",
				timeout, expect.Escalate)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		phase, phaseSince := "submitted", start
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			ack, err := edgeFetchAck(ctx, rt, nonce)
			switch {
			case err != nil && !errors.Is(err, edge.ErrNotFound):
				// The store could not be consulted. That is unknown, not
				// failure, and must not end the wait.
				fmt.Printf("%s | store unreachable, status unknown: %v\n",
					expect.Progress(time.Since(start), time.Since(phaseSince), phase), err)
			case ack != nil:
				if ack.Phase != "" && ack.Phase != phase {
					phase, phaseSince = ack.Phase, time.Now()
				}
				if ack.Accepted || ack.ReasonCode != "" {
					return printEdgeAck(ack, time.Since(start))
				}
			}

			verdict := expect.Judge(time.Since(start), time.Since(phaseSince))
			fmt.Println(expect.Progress(time.Since(start), time.Since(phaseSince), phase))
			if verdict.Escalate {
				return fmt.Errorf("edge wait: %s", verdict.Text)
			}

			select {
			case <-ctx.Done():
				fmt.Printf("\nNo acknowledgement after %s. This is unknown, not failure: "+
					"the hub may not have polled yet.\n", time.Since(start).Round(time.Second))
				fmt.Printf("Resume with: weft edge wait %s --timeout %s\n", nonce, timeout)
				return nil
			case <-ticker.C:
			}
		}
	},
}

// edgeExpectation builds the envelope for a class, falling back per field.
//
// Each trigger falls back independently. A class that configures only
// typical_minutes would otherwise leave both triggers at zero, and Judge skips
// a zero trigger — so the wait could never escalate, which is precisely the
// failure the expectation protocol exists to prevent. An operator tuning one
// number must not silently switch off the verdict.
func edgeExpectation(cfg *config.Config, class string) *edge.Expectation {
	def := edge.DefaultExpectation(class, nil)
	ec, ok := cfg.Edge.Expect[class]
	if !ok {
		return def
	}
	pick := func(configured float64, fallback time.Duration) time.Duration {
		if configured > 0 {
			return time.Duration(configured * float64(time.Minute))
		}
		return fallback
	}
	return edge.NewExpectation(class,
		pick(ec.TypicalMinutes, def.Typical),
		pick(ec.EscalateAfterMinutes, def.Escalate),
		pick(ec.StallAfterMinutes, def.Stall),
		nil)
}

func printEdgeAck(ack *edge.Ack, elapsed time.Duration) error {
	if ack.Accepted {
		fmt.Printf("\nAccepted after %s", elapsed.Round(time.Second))
		if ack.JobID != 0 {
			fmt.Printf(" as job wj%d", ack.JobID)
		}
		fmt.Println(".")
		return nil
	}
	fmt.Printf("\nRefused after %s: %s\n", elapsed.Round(time.Second), ack.Detail)
	return fmt.Errorf("submission refused: %s", ack.ReasonCode)
}

var edgeKeyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage edge signing keys",
}

var edgeKeyMintCmd = &cobra.Command{
	Use:   "mint",
	Short: "Generate this host's signing key for a plan and print its public half",
	Long: "Run on an EDGE host, normally over SSH from the hub:\n\n" +
		"  ssh agent@<edge> 'weft edge key mint --plan <plan-id>'\n\n" +
		"Prints only the public key. The private half is written mode 0600 outside any\n" +
		"synced tree and never moves. Idempotent per plan: an existing key is printed\n" +
		"rather than replaced.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		plan, _ := cmd.Flags().GetString("plan")
		if plan == "" {
			return fmt.Errorf("--plan is required: a key is minted per plan execution")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if !cfg.Edge.IsEdge() {
			return fmt.Errorf("`edge key mint` runs on an edge host; this installation's " +
				"role is hub.\nSet role = \"edge\" under [edge] to mint keys here")
		}
		// No os.Hostname() fallback. The key id is derived from the host name
		// on both sides, and the hub derives it from weft's own host namespace
		// (the short inventory names under ~/.config/weft/hosts/), while
		// os.Hostname() returns a system name like "Olivers-MacBook.local".
		// Falling back would mint a key id the hub can never register, and the
		// idempotency guard would then refuse to correct it.
		host := cfg.Edge.SubmitterHost
		if host == "" {
			return fmt.Errorf(
				"submitter_host is required under [edge] before minting a key.\n" +
					"It must be this host's weft inventory name (the one the hub will pass to " +
					"`weft edge key add --host`), not the system host name")
		}
		path := edgePlanKeyPath(cfg, plan)
		if err := edge.GuardSigningKeyPath(path); err != nil {
			return err
		}

		// Idempotent per plan. Replacing an existing key would orphan the one
		// the hub already registered, and the edge would then sign with a key
		// the hub has never seen — surfacing as a bad-signature refusal whose
		// message points nowhere near the cause.
		keyID := edge.KeyIDFor(host, plan)
		if signer, err := edge.LoadSigner(path); err == nil {
			// Idempotent for THIS plan only. A stored key belonging to another
			// plan would be printed under this plan's name, and the hub would
			// then grant this plan's budget to work signed by the other one.
			if signer.KeyID() != keyID {
				return fmt.Errorf(
					"the key at %s belongs to %q, not to plan %q (key id %q).\n"+
						"Refusing to print another plan's key under this plan's name",
					path, signer.KeyID(), plan, keyID)
			}
			fmt.Println(signer.PublicKey())
			return nil
		} else if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
			return err
		}

		signer, pub, err := edge.GenerateSigner(keyID, host)
		if err != nil {
			return err
		}
		if err := edge.SaveSigner(path, signer, host); err != nil {
			return err
		}
		fmt.Println(pub.PublicKey)
		return nil
	},
}

// edgePlanKeyPath is always plan-scoped. A fixed per-host path would make the
// key an edge signs with independent of the plan it was minted for, while the
// hub derives the key id from the plan — so the two would disagree, and the
// edge would sign under another plan's identity and spend grant.
func edgePlanKeyPath(cfg *config.Config, plan string) string {
	return filepath.Join(edgeKeyDir(cfg), plan, "signing-key.json")
}

func edgeKeyDir(cfg *config.Config) string {
	if cfg.Edge.SigningKeyDir != "" {
		return cfg.Edge.SigningKeyDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "edge-keys"
	}
	return filepath.Join(home, ".config", "weft", "edge-keys")
}

var edgeKeyAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Register an edge's public key on this hub",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		host, _ := cmd.Flags().GetString("host")
		plan, _ := cmd.Flags().GetString("plan")
		pubKey, _ := cmd.Flags().GetString("public-key")
		ceiling, _ := cmd.Flags().GetFloat64("spend-ceiling")
		project, _ := cmd.Flags().GetString("project")
		if host == "" || pubKey == "" || project == "" {
			return fmt.Errorf("--host, --project, and --public-key are required")
		}
		if _, err := base64.StdEncoding.DecodeString(pubKey); err != nil {
			return fmt.Errorf("--public-key is not valid base64: %w", err)
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if plan == "" {
			return fmt.Errorf("--plan is required: a key grants spend to one plan execution")
		}
		lease, err := edgeLeaseConfig(cfg)
		if err != nil {
			return err
		}
		now := time.Now()
		key := edge.Key{
			KeyID:           edge.KeyIDFor(host, plan),
			Host:            host,
			PlanID:          plan,
			Project:         project,
			PublicKey:       pubKey,
			NotBefore:       now,
			NotAfter:        now.Add(lease.Window),
			SpendCeilingUSD: ceiling,
		}
		// Register through the keyring rather than writing the file directly.
		// Add is the only validator, and it is what refuses to resurrect a key
		// that was revoked as compromised.
		dir := edgeKeyringDir(cfg)
		ring, err := edge.LoadKeyring(dir)
		if err != nil {
			return err
		}
		if err := ring.Add(key); err != nil {
			return err
		}
		if err := edge.SaveKey(dir, key); err != nil {
			return err
		}
		fmt.Printf("Registered %s for host %s until %s",
			key.KeyID, host, util.FormatCLITime(key.NotAfter, "2006-01-02 15:04:05"))
		if ceiling > 0 {
			fmt.Printf(", spend ceiling $%.2f", ceiling)
		}
		fmt.Println(".")
		fmt.Println("The hub daemon renews this key while the project's ownership is settled on this host.")
		return nil
	},
}

var edgeKeyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List keys this hub accepts",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ring, err := edge.LoadKeyring(edgeKeyringDir(cfg))
		if err != nil {
			return err
		}
		keys := ring.List()
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			for i := range keys {
				key := &keys[i]
				key.NotBefore = key.NotBefore.UTC()
				key.NotAfter = key.NotAfter.UTC()
				if key.RevokedAt != nil {
					revokedAt := key.RevokedAt.UTC()
					key.RevokedAt = &revokedAt
				}
			}
			// Machine-readable so a consumer reads the facts rather than
			// parsing the display line, which carries the same values in a
			// shape that is not a contract.
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(edgeKeyListJSON{
				SchemaVersion: "weft.edge-keys/v1",
				Keys:          keys,
				Problems:      ring.Problems,
			})
		}
		printKeyringProblems(ring)
		if len(keys) == 0 {
			fmt.Println("No edge keys registered.")
			return nil
		}
		now := time.Now()
		for _, k := range keys {
			fmt.Println(describeKey(k, now))
		}
		return nil
	},
}

var edgeKeyRenewCmd = &cobra.Command{
	Use:   "renew <key-id>",
	Short: "Slide a key's admission window forward",
	Long: "Renews only while research-site reports the key's project under settled remote\n" +
		"ownership on the key's host. The check and renewal run on the hub; the edge\n" +
		"never learns the window exists.\n\n" +
		"Not renewing is how a plan ends. Its authority lapses on its own.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		dir := edgeKeyringDir(cfg)
		ring, err := edge.LoadKeyring(dir)
		if err != nil {
			return err
		}
		lease, err := edgeLeaseConfig(cfg)
		if err != nil {
			return err
		}
		report := renewEdgeKeyOnce(cmd.Context(), ring, args[0], lease, time.Now(), runResearchSiteStatus)
		for _, problem := range report.Problems {
			fmt.Fprintf(os.Stderr, "edge key renewal warning: %s\n", secrets.RedactText(problem))
		}
		for _, deferred := range report.Deferred {
			fmt.Fprintf(os.Stderr, "edge key renewal deferred: %s\n", secrets.RedactText(deferred))
		}
		if len(report.Renewed) != 1 {
			reasons := append(append([]string{}, report.Problems...), report.Deferred...)
			if len(reasons) == 0 {
				reasons = append(reasons, "ownership did not authorize renewal")
			}
			return fmt.Errorf("renew key %s: %s", args[0], secrets.RedactText(strings.Join(reasons, "; ")))
		}
		key, _ := ring.Lookup(args[0])
		fmt.Printf("Renewed %s until %s.\n", key.KeyID, util.FormatCLITime(key.NotAfter, "2006-01-02 15:04:05"))
		return nil
	},
}

var edgeKeyRevokeCmd = &cobra.Command{
	Use:   "revoke <key-id>",
	Short: "End a key's authority, or repudiate it as compromised",
	Long: "By default this ends the key's admission window: nothing new is accepted, and\n" +
		"everything already admitted stands and stays verifiable. This is how a plan\n" +
		"ends.\n\n" +
		"--compromised additionally repudiates the key's past signatures, so work it\n" +
		"already submitted stops being evidence of anything. Use it only when the key\n" +
		"is believed stolen; it cannot be undone.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		compromised, _ := cmd.Flags().GetBool("compromised")
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		dir := edgeKeyringDir(cfg)
		ring, err := edge.LoadKeyring(dir)
		if err != nil {
			return err
		}
		now := time.Now()
		if compromised {
			if err := ring.Revoke(args[0], now); err != nil {
				return err
			}
		} else if err := ring.EndAuthority(args[0], now); err != nil {
			return err
		}
		key, _ := ring.Lookup(args[0])
		if err := edge.SaveKey(dir, key); err != nil {
			return err
		}
		if compromised {
			fmt.Printf("Revoked %s as compromised. Its past signatures are no longer "+
				"treated as evidence.\n", key.KeyID)
		} else {
			fmt.Printf("Ended authority for %s. Work it already submitted stands.\n", key.KeyID)
		}
		return nil
	},
}

// printKeyringProblems reports key files that could not be loaded. They are
// skipped rather than fatal, so a malformed row cannot disable the commands an
// operator needs to fix it — but staying silent would hide a key the hub is
// no longer accepting.
func printKeyringProblems(ring *edge.Keyring) {
	if ring == nil || len(ring.Problems) == 0 {
		return
	}
	fmt.Printf("WARNING: %d key file(s) could not be loaded and are being ignored:\n",
		len(ring.Problems))
	for _, p := range ring.Problems {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println()
}

// edgeLeaseConfig builds the lease from configuration and validates it.
//
// One helper for every caller — add, renew, and runtime assembly — so they
// cannot disagree about what a valid lease configuration is.
func edgeLeaseConfig(cfg *config.Config) (edge.LeaseConfig, error) {
	lease := edge.DefaultLeaseConfig()
	if cfg.Edge.LeaseWindowHours > 0 {
		lease.Window = time.Duration(cfg.Edge.LeaseWindowHours * float64(time.Hour))
	}
	if cfg.Edge.LeaseRenewIntervalHours > 0 {
		lease.Interval = time.Duration(cfg.Edge.LeaseRenewIntervalHours * float64(time.Hour))
	}
	if err := lease.Validate(); err != nil {
		return edge.LeaseConfig{}, err
	}
	return lease, nil
}

// edgeKeyListJSON is the machine-readable shape of `weft edge key list --json`.
//
// Problems is included rather than printed to stderr because a consumer
// deciding whether to renew a lease must be able to tell "this key is absent"
// from "this key file could not be read" — the same false-absence distinction
// the rest of this package keeps.
type edgeKeyListJSON struct {
	SchemaVersion string     `json:"schema_version"`
	Keys          []edge.Key `json:"keys"`
	Problems      []string   `json:"problems,omitempty"`
}
