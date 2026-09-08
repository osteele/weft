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
)

// edgeSubmitServed is the subset of submit-classified commands with a payload
// and hub-side handler in this build. Every other submit command remains at
// the edge gate with edgeCauseNoSubmitPath.
var edgeSubmitServed = map[string]struct{}{
	"run":     {},
	"job run": {},
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
