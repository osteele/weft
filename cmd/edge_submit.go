package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// edgeSubmitServed is the subset of submit-classified commands with a payload
// and hub-side handler in this build. Every other submit command remains at
// the edge gate with edgeCauseNoSubmitPath.
var edgeSubmitServed = map[string]struct{}{
	"bug note":             {},
	"bug report":           {},
	"cancel":               {},
	"edit":                 {},
	"job cancel":           {},
	"job kill":             {},
	"job mark-processed":   {},
	"job mark-unprocessed": {},
	"job pause":            {},
	"job restart":          {},
	"job resume":           {},
	"job run":              {},
	"job unpause":          {},
	"kill":                 {},
	"mark-processed":       {},
	"mark-unprocessed":     {},
	"pause":                {},
	"pause job":            {},
	"queue edit":           {},
	"restart":              {},
	"resume":               {},
	"run":                  {},
	"unpause":              {},
	"unpause job":          {},
}

var activeEdgeSubmit *edge.Runtime

func deploymentSourceDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate submitting executable: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open submitting executable: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash submitting executable: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func submitEdgeJob(ctx context.Context, out io.Writer, rt *edge.Runtime, cfg *config.Config, params ops.QueueJobParams, sourceDigest string, sourceClosure []byte) error {
	queueParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode job request: %w", err)
	}
	spend := 0.0
	if params.CLIOverrides != nil && params.CLIOverrides.MaxSpendCents != nil {
		spend = float64(*params.CLIOverrides.MaxSpendCents) / 100
	}
	hosts := []string(nil)
	if params.Host != "" {
		hosts = []string{params.Host}
	}
	payload, err := edge.EncodeWeftJobPayload(edge.WeftJobPayload{
		Command:           params.Command,
		WorkingDir:        params.WorkingDir,
		Project:           params.Project,
		Description:       params.Description,
		SourceDigest:      sourceDigest,
		QueueParams:       queueParams,
		SpendCeilingUSD:   spend,
		TargetConstraints: edge.TargetConstraints{Hosts: hosts, GPU: params.GPUClass, Tags: params.Tags},
	})
	if err != nil {
		return err
	}
	deploymentDigest, err := deploymentSourceDigest()
	if err != nil {
		return err
	}
	result, err := edge.Submit(ctx, rt.Transport, rt.Signer, edge.SubmitRequest{
		SubmitterHost:          rt.SubmitterHost,
		DeploymentSourceDigest: deploymentDigest,
		Kind:                   edge.KindWeftJobSubmission,
		Payload:                payload,
		SourceClosure:          sourceClosure,
		SourceDigest:           sourceDigest,
	}, time.Now())
	if err != nil {
		return err
	}

	wait := cfg.Edge.AdmissionWait()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ack, err := edgeFetchAck(waitCtx, rt, result.Nonce)
		switch {
		case err == nil && ack != nil && ack.Accepted && ack.JobID > 0:
			_, err = fmt.Fprintln(out, ids.FormatJobID(ack.JobID))
			return err
		case err == nil && ack != nil && ack.ReasonCode != "":
			if _, writeErr := fmt.Fprintf(out, "Refused: %s: %s\n", ack.ReasonCode, ack.Detail); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("submission refused: %s", ack.ReasonCode)
		case err != nil && !errors.Is(err, edge.ErrNotFound) && !errors.Is(err, context.DeadlineExceeded):
			// The submission pointer is durable once Submit returns. A read-side
			// outage cannot turn that accepted write into a failed submission.
		}

		select {
		case <-waitCtx.Done():
			if _, err := fmt.Fprintf(out,
				"Submission %s stands; the job id is assigned on admission. No acknowledgement arrived within %s.\nWait with: weft edge wait %s\n",
				result.Nonce, wait, result.Nonce); err != nil {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}

func changedCommandFlags(cmd *cobra.Command) map[string]string {
	flags := map[string]string{}
	rootPersistent := cmd.Root().PersistentFlags()
	cmd.NonInheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if !flag.Changed || rootPersistent.Lookup(flag.Name) != nil {
			return
		}
		flags[flag.Name] = flag.Value.String()
	})
	if len(flags) == 0 {
		return nil
	}
	return flags
}

func edgeControlSpendCeilingUSD(action edge.JobControlAction, flags map[string]string) (float64, error) {
	if action != edge.ControlEdit {
		return 0, nil
	}
	raw, ok := flags["max-spend"]
	if !ok {
		return 0, nil
	}
	cents, err := parseDollarCap("max-spend", raw)
	if err != nil {
		return 0, err
	}
	if cents == nil {
		return 0, nil
	}
	return float64(*cents) / 100, nil
}

func submitEdgeJobControls(cmd *cobra.Command, args []string, action edge.JobControlAction, parser func([]string) ([]int64, error)) error {
	jobIDs, err := parser(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	flags := changedCommandFlags(cmd)
	spendCeilingUSD, err := edgeControlSpendCeilingUSD(action, flags)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		requestID, err := edge.NewNonce(time.Now())
		if err != nil {
			return fmt.Errorf("create job-control request identity: %w", err)
		}
		payload, err := edge.EncodeWeftJobControlPayload(edge.WeftJobControlPayload{
			RequestID:       requestID,
			JobID:           jobID,
			Action:          action,
			Flags:           flags,
			SpendCeilingUSD: spendCeilingUSD,
		})
		if err != nil {
			return err
		}
		if err := submitEdgePayloadAndWait(cmd.Context(), cmd.OutOrStdout(), activeEdgeSubmit, cfg,
			edge.KindWeftJobControl, payload, fmt.Sprintf("Job %s: %s completed", ids.FormatJobID(jobID), action)); err != nil {
			return err
		}
	}
	return nil
}

func submitEdgeBugRecord(cmd *cobra.Command, payload edge.WeftBugReportPayload) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	data, err := edge.EncodeWeftBugReportPayload(payload)
	if err != nil {
		return err
	}
	return submitEdgePayloadAndWait(cmd.Context(), cmd.OutOrStdout(), activeEdgeSubmit, cfg,
		edge.KindWeftBugReport, data, "Bug record accepted")
}

func submitEdgePayloadAndWait(ctx context.Context, out io.Writer, rt *edge.Runtime, cfg *config.Config, kind edge.PayloadKind, payload []byte, fallback string) error {
	deploymentDigest, err := deploymentSourceDigest()
	if err != nil {
		return err
	}
	result, err := edge.Submit(ctx, rt.Transport, rt.Signer, edge.SubmitRequest{
		SubmitterHost:          rt.SubmitterHost,
		DeploymentSourceDigest: deploymentDigest,
		Kind:                   kind,
		Payload:                payload,
	}, time.Now())
	if err != nil {
		return err
	}

	wait := cfg.Edge.AdmissionWait()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ack, err := edgeFetchAck(waitCtx, rt, result.Nonce)
		switch {
		case err == nil && ack != nil && ack.Accepted:
			detail := ack.Detail
			if detail == "" {
				detail = fallback
			}
			_, err = fmt.Fprintln(out, detail)
			return err
		case err == nil && ack != nil && ack.ReasonCode != "":
			if _, writeErr := fmt.Fprintf(out, "Refused: %s: %s\n", ack.ReasonCode, ack.Detail); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("submission refused: %s", ack.ReasonCode)
		case err != nil && !errors.Is(err, edge.ErrNotFound) && !errors.Is(err, context.DeadlineExceeded):
			// The committed pointer remains authoritative. A failed acknowledgement
			// read leaves the result unknown and cannot turn it into a failure.
		}

		select {
		case <-waitCtx.Done():
			if _, err := fmt.Fprintf(out,
				"Submission %s stands. No acknowledgement arrived within %s.\nWait with: weft edge wait %s\n",
				result.Nonce, wait, result.Nonce); err != nil {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}
