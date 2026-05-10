package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

const restEndpoint = "https://rest.runpod.io/v1"

var fetchPodDetailFunc = fetchPodDetail

// FetchPodDetail fetches a RunPod pod through REST with machine metadata
// included when RunPod still exposes the pod.
func FetchPodDetail(ctx context.Context, podID string) (*Pod, error) {
	return fetchPodDetail(ctx, podID)
}

func fetchPodDetail(ctx context.Context, podID string) (*Pod, error) {
	apiKey, err := readAPIKey()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(restEndpoint + "/pods/" + url.PathEscape(podID))
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("includeMachine", "true")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runpod rest pod: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: runpod rest pod %s", cloud.ErrInstanceNotFound, podID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("runpod rest pod %s: status %d", podID, resp.StatusCode)
	}

	var raw any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("runpod rest pod decode: %w", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("runpod rest pod decode: expected object")
	}
	pod := podFromMap(unwrapPodObject(obj))
	return &pod, nil
}

func unwrapPodObject(obj map[string]any) map[string]any {
	for _, key := range []string{"pod", "data"} {
		nested, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstString(nested, "id", "podId")) != "" {
			return nested
		}
	}
	return obj
}
