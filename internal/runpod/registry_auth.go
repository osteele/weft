package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

const registryAuthEndpoint = "https://rest.runpod.io/v1/containerregistryauth"

var registryAuthEndpointURL = registryAuthEndpoint
var registryAuthHTTPClient = &http.Client{Timeout: 20 * time.Second}

type registryAuthRecord struct {
	ID          string `json:"id"`
	RegistryID  string `json:"registryAuthId"`
	RegistryID2 string `json:"containerRegistryAuthId"`
	Name        string `json:"name"`
	RegistryURL string `json:"registryUrl"`
}

func ensureRegistryAuth(ctx context.Context, auth *cloud.RegistryAuth) (string, error) {
	if auth == nil {
		return "", nil
	}
	name := strings.TrimSpace(auth.Name)
	if name == "" {
		name = "weft-" + strings.TrimPrefix(strings.ReplaceAll(auth.Host, ".", "-"), "-")
	}
	apiKey, err := readAPIKey()
	if err != nil {
		return "", err
	}
	records, err := listRegistryAuths(ctx, apiKey)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Name == name {
			if id := recordID(record); id != "" {
				return id, nil
			}
		}
	}
	return createRegistryAuth(ctx, apiKey, name, auth)
}

func listRegistryAuths(ctx context.Context, apiKey string) ([]registryAuthRecord, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", registryAuthEndpointURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := registryAuthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list registry auths: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list registry auths: HTTP %d", resp.StatusCode)
	}
	var raw any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode registry auth list: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("list registry auths: response is null")
	}
	records := decodeRegistryAuthRecords(raw)
	if records == nil {
		return nil, fmt.Errorf("list registry auths: unrecognized response shape")
	}
	return records, nil
}

func createRegistryAuth(ctx context.Context, apiKey, name string, auth *cloud.RegistryAuth) (string, error) {
	body, err := json.Marshal(map[string]string{
		"name":        name,
		"registryUrl": auth.Host,
		"username":    auth.Username,
		"password":    auth.Password,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", registryAuthEndpointURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := registryAuthHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create registry auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("create registry auth: HTTP %d", resp.StatusCode)
	}
	var record registryAuthRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return "", fmt.Errorf("decode registry auth create: %w", err)
	}
	id := recordID(record)
	if id == "" {
		return "", fmt.Errorf("create registry auth returned no id")
	}
	return id, nil
}

func recordID(record registryAuthRecord) string {
	for _, id := range []string{record.ID, record.RegistryID, record.RegistryID2} {
		if strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func decodeRegistryAuthRecords(raw any) []registryAuthRecord {
	switch v := raw.(type) {
	case []any:
		return decodeRegistryAuthRecordList(v)
	case map[string]any:
		for _, key := range []string{"containerRegistryAuths", "registryAuths", "items", "data"} {
			if list, ok := v[key].([]any); ok {
				return decodeRegistryAuthRecordList(list)
			}
		}
	}
	return nil
}

func decodeRegistryAuthRecordList(items []any) []registryAuthRecord {
	records := make([]registryAuthRecord, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var record registryAuthRecord
		if err := json.Unmarshal(data, &record); err == nil {
			records = append(records, record)
		}
	}
	return records
}
