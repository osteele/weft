package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/secrets"
)

const researchSiteStatusSchema = "research-site.status/v1"

type researchSiteStatus struct {
	SchemaVersion string                `json:"schema_version"`
	Projects      []researchSiteProject `json:"projects"`
}

type researchSiteProject struct {
	Project    string `json:"project"`
	OwnedBy    string `json:"owned_by"`
	Settled    bool   `json:"settled"`
	RemoteHost string `json:"remote_host"`
}

type researchSiteStatusRunner func(context.Context) (stdout []byte, stderr string, err error)

type edgeRenewalReport struct {
	Renewed  []string
	Deferred []string
	Problems []string
}

func shouldRenewEdgeKeys(cfg *config.Config) bool {
	return cfg != nil && !cfg.Edge.IsEdge()
}

func runEdgeKeyRenewer(ctx context.Context, cfg *config.Config) {
	lease, err := edgeLeaseConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "edge key renewal disabled: %s\n", secrets.RedactText(err.Error()))
		return
	}

	run := func() {
		report := renewEdgeKeysOnce(ctx, edgeKeyringDir(cfg), lease, time.Now(), runResearchSiteStatus)
		for _, problem := range report.Problems {
			fmt.Fprintf(os.Stderr, "edge key renewal warning: %s\n", secrets.RedactText(problem))
		}
		for _, deferred := range report.Deferred {
			fmt.Fprintf(os.Stderr, "edge key renewal deferred: %s\n", secrets.RedactText(deferred))
		}
		if len(report.Renewed) > 0 {
			fmt.Fprintf(os.Stderr, "edge key renewal: renewed %s\n", strings.Join(report.Renewed, ", "))
		}
	}

	run()
	ticker := time.NewTicker(lease.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func runResearchSiteStatus(ctx context.Context) ([]byte, string, error) {
	command := exec.CommandContext(ctx, "research-site", "status", "--json")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

func renewEdgeKeysOnce(
	ctx context.Context,
	keyringDir string,
	lease edge.LeaseConfig,
	now time.Time,
	runStatus researchSiteStatusRunner,
) edgeRenewalReport {
	var report edgeRenewalReport
	if err := lease.Validate(); err != nil {
		report.Problems = append(report.Problems, err.Error())
		return report
	}

	ring, err := edge.LoadKeyring(keyringDir)
	if err != nil {
		report.Problems = append(report.Problems, fmt.Sprintf("load keyring: %v", err))
		return report
	}
	report.Problems = append(report.Problems, ring.Problems...)

	keys := keysDueForRenewal(ring.List(), lease, now, &report)
	if len(keys) == 0 {
		return report
	}
	return renewAuthorizedEdgeKeys(ctx, ring, keys, lease, now, runStatus, report)
}

func renewEdgeKeyOnce(
	ctx context.Context,
	ring *edge.Keyring,
	keyID string,
	lease edge.LeaseConfig,
	now time.Time,
	runStatus researchSiteStatusRunner,
) edgeRenewalReport {
	var report edgeRenewalReport
	if err := lease.Validate(); err != nil {
		report.Problems = append(report.Problems, err.Error())
		return report
	}
	if ring == nil {
		report.Problems = append(report.Problems, "no keyring")
		return report
	}
	report.Problems = append(report.Problems, ring.Problems...)
	key, ok := ring.Lookup(keyID)
	if !ok {
		report.Problems = append(report.Problems, fmt.Sprintf("no key with id %q", keyID))
		return report
	}
	return renewAuthorizedEdgeKeys(ctx, ring, []edge.Key{key}, lease, now, runStatus, report)
}

func renewAuthorizedEdgeKeys(
	ctx context.Context,
	ring *edge.Keyring,
	keys []edge.Key,
	lease edge.LeaseConfig,
	now time.Time,
	runStatus researchSiteStatusRunner,
	report edgeRenewalReport,
) edgeRenewalReport {
	stdout, stderr, commandErr := runStatus(ctx)
	status, parseErr := parseResearchSiteStatus(stdout)
	if parseErr != nil {
		report.Problems = append(report.Problems, fmt.Sprintf("read project ownership: %v", parseErr))
		if commandErr != nil {
			report.Problems = append(report.Problems, describeResearchSiteFailure(commandErr, stderr))
		}
		return report
	}
	if commandErr != nil {
		// research-site may return a non-zero status while still reporting healthy
		// rows for independent projects. The versioned stdout remains usable; the
		// process failure is retained as a diagnostic rather than discarding it.
		report.Problems = append(report.Problems, describeResearchSiteFailure(commandErr, stderr))
	}

	projects := make(map[string][]researchSiteProject, len(status.Projects))
	for _, project := range status.Projects {
		if project.Project == "" {
			report.Problems = append(report.Problems, "research-site returned a project row with no project name")
			continue
		}
		projects[project.Project] = append(projects[project.Project], project)
	}

	for _, key := range keys {
		rows := projects[key.Project]
		if len(rows) == 0 {
			report.Problems = append(report.Problems,
				fmt.Sprintf("key %s: project %q is absent from research-site status; ownership is unknown", key.KeyID, key.Project))
			continue
		}
		if len(rows) != 1 {
			report.Problems = append(report.Problems,
				fmt.Sprintf("key %s: project %q has %d status rows; ownership is ambiguous", key.KeyID, key.Project, len(rows)))
			continue
		}
		project := rows[0]
		switch {
		case !project.Settled:
			report.Deferred = append(report.Deferred,
				fmt.Sprintf("key %s: project %q has a handoff in flight", key.KeyID, key.Project))
		case project.OwnedBy != "remote":
			report.Deferred = append(report.Deferred,
				fmt.Sprintf("key %s: project %q is owned by %q, not remote", key.KeyID, key.Project, project.OwnedBy))
		case normalizedSSHHost(project.RemoteHost) == "":
			report.Problems = append(report.Problems,
				fmt.Sprintf("key %s: project %q has no remote host; ownership target is unknown", key.KeyID, key.Project))
		case normalizedSSHHost(project.RemoteHost) != normalizedSSHHost(key.Host):
			report.Problems = append(report.Problems,
				fmt.Sprintf("key %s: project %q is remote on %q, not key host %q", key.KeyID, key.Project, project.RemoteHost, key.Host))
		default:
			if err := ring.Renew(key.KeyID, now, lease); err != nil {
				report.Problems = append(report.Problems, fmt.Sprintf("renew key %s: %v", key.KeyID, err))
				continue
			}
			report.Renewed = append(report.Renewed, key.KeyID)
		}
	}
	return report
}

func keysDueForRenewal(keys []edge.Key, lease edge.LeaseConfig, now time.Time, report *edgeRenewalReport) []edge.Key {
	due := make([]edge.Key, 0, len(keys))
	for _, key := range keys {
		switch {
		case key.RevokedAt != nil:
			continue
		case key.Project == "":
			if !key.NotAfter.IsZero() && !now.After(key.NotAfter) && renewalDue(key, lease, now) {
				report.Problems = append(report.Problems,
					fmt.Sprintf("key %s has no project; automatic renewal cannot establish ownership", key.KeyID))
			}
		case key.NotAfter.IsZero():
			report.Problems = append(report.Problems,
				fmt.Sprintf("key %s has no validity deadline; automatic renewal cannot bound its authority", key.KeyID))
		case now.After(key.NotAfter):
			continue
		case renewalDue(key, lease, now):
			due = append(due, key)
		}
	}
	return due
}

func renewalDue(key edge.Key, lease edge.LeaseConfig, now time.Time) bool {
	return !key.NotAfter.After(now.Add(lease.Window - lease.Interval/2))
}

func parseResearchSiteStatus(data []byte) (researchSiteStatus, error) {
	var status researchSiteStatus
	if len(bytes.TrimSpace(data)) == 0 {
		return status, fmt.Errorf("research-site status returned no JSON")
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return status, fmt.Errorf("decode research-site status JSON: %w", err)
	}
	if status.SchemaVersion != researchSiteStatusSchema {
		return status, fmt.Errorf("research-site status schema is %q, want %q", status.SchemaVersion, researchSiteStatusSchema)
	}
	if status.Projects == nil {
		return status, fmt.Errorf("research-site status omitted projects")
	}
	return status, nil
}

func describeResearchSiteFailure(err error, stderr string) string {
	if stderr == "" {
		return fmt.Sprintf("research-site status failed: %v", err)
	}
	return fmt.Sprintf("research-site status failed: %v: %s", err, stderr)
}

func normalizedSSHHost(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.LastIndexByte(value, '@'); at >= 0 {
		value = value[at+1:]
	}
	return strings.TrimSuffix(strings.ToLower(value), ".")
}
