package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml"
)

const graphqlEndpoint = "https://api.runpod.io/graphql"

// gpuTypesQuery fetches all GPU types with pricing via the RunPod GraphQL API.
// The REST-like `runpodctl gpu list` command does not include pricing, so we
// must query GraphQL directly for offer construction.
func gpuTypesQuery(cloudType string) string {
	secureCloud := "false"
	if cloudType == "secure" {
		secureCloud = "true"
	}
	return fmt.Sprintf(`{
  gpuTypes {
    id
    displayName
    memoryInGb
    secureCloud
    communityCloud
    maxGpuCount
    lowestPrice(input: {gpuCount: 1, secureCloud: %s}) {
      minimumBidPrice
      uninterruptablePrice
      stockStatus
    }
  }
  dataCenters {
    id
    gpuAvailability {
      gpuTypeId
      stockStatus
    }
  }
}`, secureCloud)
}

type gqlGPUType struct {
	ID             string          `json:"id"`
	DisplayName    string          `json:"displayName"`
	MemoryInGb     int             `json:"memoryInGb"`
	SecureCloud    bool            `json:"secureCloud"`
	CommunityCloud bool            `json:"communityCloud"`
	MaxGPUCount    int             `json:"maxGpuCount"`
	LowestPrice    *gqlLowestPrice `json:"lowestPrice"`
}

type gqlLowestPrice struct {
	MinimumBidPrice      *float64 `json:"minimumBidPrice"`
	UninterruptablePrice *float64 `json:"uninterruptablePrice"`
	StockStatus          *string  `json:"stockStatus"`
}

type gqlDataCenter struct {
	ID              string `json:"id"`
	GPUAvailability []struct {
		GPUTypeID   string  `json:"gpuTypeId"`
		StockStatus *string `json:"stockStatus"`
	} `json:"gpuAvailability"`
}

type gqlResponse struct {
	Data struct {
		GPUTypes    []gqlGPUType    `json:"gpuTypes"`
		DataCenters []gqlDataCenter `json:"dataCenters"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// readAPIKey returns the RunPod API key from $RUNPOD_API_KEY or ~/.runpod/config.toml.
func readAPIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("RUNPOD_API_KEY")); key != "" {
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := home + "/.runpod/config.toml"
	tree, err := toml.LoadFile(path)
	if err != nil {
		return "", fmt.Errorf("read runpod api key: %w", err)
	}
	raw := tree.GetPath([]string{"default", "api_key"})
	key, _ := raw.(string)
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("runpod api key not found; set RUNPOD_API_KEY or run `runpodctl doctor`")
	}
	return key, nil
}

// fetchGPUTypes queries the RunPod GraphQL API for GPU types with pricing and
// per-datacenter availability. The `gpuTypes.lowestPrice.stockStatus` is often
// stale/aggregated; the `dataCenters.gpuAvailability` list is authoritative for
// which GPU types can actually be rented right now. GPU types absent from all
// datacenter availability lists are filtered out.
func fetchGPUTypes(ctx context.Context, cloudType string) ([]gqlGPUType, error) {
	apiKey, err := readAPIKey()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]string{"query": gpuTypesQuery(cloudType)})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runpod graphql: %w", err)
	}
	defer resp.Body.Close()

	var gql gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gql); err != nil {
		return nil, fmt.Errorf("runpod graphql decode: %w", err)
	}
	if len(gql.Errors) > 0 {
		msgs := make([]string, len(gql.Errors))
		for i, e := range gql.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("runpod graphql: %s", strings.Join(msgs, "; "))
	}

	// Build the set of GPU type IDs that are actually available in at least
	// one datacenter with non-empty stock. gpuTypes.lowestPrice.stockStatus
	// often reports "Low" for types with zero actual capacity, which causes
	// pod creation to fail with "no instances available".
	available := make(map[string]bool)
	for _, dc := range gql.Data.DataCenters {
		for _, avail := range dc.GPUAvailability {
			if avail.StockStatus != nil && *avail.StockStatus != "" {
				available[avail.GPUTypeID] = true
			}
		}
	}

	// If the datacenters query came back empty, fall back to unfiltered list.
	if len(available) == 0 {
		return gql.Data.GPUTypes, nil
	}

	filtered := gql.Data.GPUTypes[:0]
	for _, gt := range gql.Data.GPUTypes {
		if available[gt.ID] {
			filtered = append(filtered, gt)
		}
	}
	return filtered, nil
}
