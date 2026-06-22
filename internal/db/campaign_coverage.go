package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// ProviderMachineKey returns the offer-claim key for a provider machine.
func ProviderMachineKey(provider, machineID string) string {
	provider = strings.TrimSpace(provider)
	machineID = strings.TrimSpace(machineID)
	if provider == "" || machineID == "" {
		return ""
	}
	return provider + "/" + machineID
}

// CampaignCoveredMachineIDs returns provider-qualified machine IDs that have
// already produced a completed datapoint for this campaign.
func CampaignCoveredMachineIDs(database *sql.DB, campaignID int64) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if database == nil || campaignID <= 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT DISTINCT COALESCE(l.provider, ''), COALESCE(l.machine_id, '')
		  FROM job_attempts ja
		  JOIN launches l ON l.id = ja.launch_id
		 WHERE l.campaign_id = ?
		   AND COALESCE(l.machine_id, '') != ''
		   AND ja.cloud_outcome = ?`,
		campaignID,
		AttemptOutcomeCompleted,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if err := scanProviderMachineKeys(rows, out); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// CampaignInflightMachineIDs returns provider-qualified machine IDs for
// non-terminal launches in this campaign.
func CampaignInflightMachineIDs(database *sql.DB, campaignID int64) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if database == nil || campaignID <= 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT DISTINCT COALESCE(provider, ''), COALESCE(machine_id, '')
		  FROM launches
		 WHERE campaign_id = ?
		   AND COALESCE(machine_id, '') != ''
		   AND status NOT IN (?, ?, ?)`,
		campaignID,
		LaunchStatusCompleted,
		LaunchStatusFailed,
		LaunchStatusCancelled,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if err := scanProviderMachineKeys(rows, out); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

func scanProviderMachineKeys(rows *sql.Rows, out map[string]struct{}) error {
	for rows.Next() {
		var provider, machineID string
		if err := rows.Scan(&provider, &machineID); err != nil {
			return err
		}
		if key := ProviderMachineKey(provider, machineID); key != "" {
			out[key] = struct{}{}
		}
	}
	return nil
}

// ResolveAvoidMachineIDs resolves --avoid tokens into provider machine IDs.
// Unresolvable tokens return warnings and are omitted from the result.
func ResolveAvoidMachineIDs(database *sql.DB, tokens []string) ([]string, []string, error) {
	out := make([]string, 0, len(tokens))
	warnings := []string{}
	seen := map[string]struct{}{}
	for _, token := range splitAvoidTokens(tokens) {
		machineID, warning, err := resolveAvoidMachineID(database, token)
		if err != nil {
			return nil, nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
			continue
		}
		if machineID == "" {
			continue
		}
		if _, ok := seen[machineID]; ok {
			continue
		}
		seen[machineID] = struct{}{}
		out = append(out, machineID)
	}
	return out, warnings, nil
}

func splitAvoidTokens(tokens []string) []string {
	out := []string{}
	for _, token := range tokens {
		for _, part := range strings.Split(token, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func resolveAvoidMachineID(database *sql.DB, token string) (string, string, error) {
	lower := strings.ToLower(strings.TrimSpace(token))
	switch {
	case strings.HasPrefix(lower, "wi"):
		id, err := parsePositivePrefixedID(lower, "wi")
		if err != nil {
			return "", fmt.Sprintf("could not resolve --avoid %q: invalid instance id", token), nil
		}
		return resolveAvoidInstanceMachineID(database, token, id)
	case strings.HasPrefix(lower, "wj"):
		id, err := parsePositivePrefixedID(lower, "wj")
		if err != nil {
			return "", fmt.Sprintf("could not resolve --avoid %q: invalid job id", token), nil
		}
		return resolveAvoidJobMachineID(database, token, id)
	default:
		return strings.TrimSpace(token), "", nil
	}
}

func parsePositivePrefixedID(token, prefix string) (int64, error) {
	value := strings.TrimPrefix(token, prefix)
	if value == "" {
		return 0, fmt.Errorf("missing numeric suffix")
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func resolveAvoidInstanceMachineID(database *sql.DB, token string, instanceID int64) (string, string, error) {
	var machineID string
	err := database.QueryRow(
		`SELECT COALESCE(machine_id, '') FROM launches WHERE id = ?`,
		instanceID,
	).Scan(&machineID)
	if err == sql.ErrNoRows {
		return "", fmt.Sprintf("could not resolve --avoid %q: instance not found", token), nil
	}
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(machineID) == "" {
		return "", fmt.Sprintf("could not resolve --avoid %q: instance has no machine_id", token), nil
	}
	return strings.TrimSpace(machineID), "", nil
}

func resolveAvoidJobMachineID(database *sql.DB, token string, jobID int64) (string, string, error) {
	var machineID string
	err := database.QueryRow(`
		SELECT COALESCE(l.machine_id, '')
		  FROM job_attempts ja
		  LEFT JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.job_id = ?
		 ORDER BY ja.attempt_number DESC, ja.id DESC
		 LIMIT 1`,
		jobID,
	).Scan(&machineID)
	if err == sql.ErrNoRows {
		return "", fmt.Sprintf("could not resolve --avoid %q: job has no attempts", token), nil
	}
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(machineID) == "" {
		return "", fmt.Sprintf("could not resolve --avoid %q: latest job attempt has no machine_id", token), nil
	}
	return strings.TrimSpace(machineID), "", nil
}
