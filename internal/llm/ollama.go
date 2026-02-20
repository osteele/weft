package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultOllamaModel is the default ollama model to use
	DefaultOllamaModel = "llama3.2"

	// OllamaAPIURL is the default ollama API endpoint
	OllamaAPIURL = "http://localhost:11434/api/generate"

	// OllamaTimeout is the timeout for ollama API requests
	OllamaTimeout = 60 * time.Second

	// ollamaAvailabilityCacheTTL is how long to cache availability check results.
	// This allows re-checking if Ollama becomes available after initial failure.
	ollamaAvailabilityCacheTTL = 30 * time.Second
)

// OllamaConfig holds the configuration for Ollama
type OllamaConfig struct {
	Model   string
	APIURL  string
	Timeout time.Duration
}

// DefaultOllamaConfig returns the default Ollama configuration
func DefaultOllamaConfig() OllamaConfig {
	return OllamaConfig{
		Model:   DefaultOllamaModel,
		APIURL:  OllamaAPIURL,
		Timeout: OllamaTimeout,
	}
}

// OllamaConfigWithModel returns a config with the specified model
func OllamaConfigWithModel(model string) OllamaConfig {
	cfg := DefaultOllamaConfig()
	if model != "" {
		cfg.Model = model
	}
	return cfg
}

// ollamaRequest is the request body for ollama API
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ollamaResponse is the response from ollama API
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// OllamaClient provides access to local Ollama LLM services
type OllamaClient struct {
	config     OllamaConfig
	httpClient *http.Client

	// Availability cache with thread-safe access
	availMu       sync.RWMutex
	available     *bool     // Cached availability check
	availableTime time.Time // When availability was last checked
}

// NewOllamaClient creates a new Ollama client with the given configuration
func NewOllamaClient(config OllamaConfig) *OllamaClient {
	if config.Model == "" {
		config.Model = DefaultOllamaModel
	}
	if config.APIURL == "" {
		config.APIURL = OllamaAPIURL
	}
	if config.Timeout == 0 {
		config.Timeout = OllamaTimeout
	}

	return &OllamaClient{
		config: config,
		httpClient: &http.Client{
			Timeout: config.Timeout,
		},
	}
}

// IsAvailable checks if ollama is installed and running.
// Results are cached for ollamaAvailabilityCacheTTL to allow re-checking
// if Ollama becomes available after initial failure.
func (c *OllamaClient) IsAvailable() bool {
	// Fast path: check cache with read lock
	c.availMu.RLock()
	if c.available != nil && time.Since(c.availableTime) < ollamaAvailabilityCacheTTL {
		result := *c.available
		c.availMu.RUnlock()
		return result
	}
	c.availMu.RUnlock()

	// Slow path: perform check and update cache
	c.availMu.Lock()
	defer c.availMu.Unlock()

	// Double-check after acquiring write lock
	if c.available != nil && time.Since(c.availableTime) < ollamaAvailabilityCacheTTL {
		return *c.available
	}

	available := c.checkAvailability()
	c.available = &available
	c.availableTime = time.Now()
	return available
}

// tagsResponse is the response from the /api/tags endpoint
type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// checkAvailability performs the actual availability check (not thread-safe, called with lock held)
func (c *OllamaClient) checkAvailability() bool {
	// Check if ollama command exists
	if _, err := exec.LookPath("ollama"); err != nil {
		return false
	}

	// Check if ollama server is running and the model is available
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost:11434/api/tags", nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Check that our model is in the list of available models
	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false
	}

	for _, m := range tags.Models {
		// Model names can be "llama3.2:latest" — match with or without tag
		name := m.Name
		if strings.Contains(name, ":") {
			name = strings.Split(name, ":")[0]
		}
		if name == c.config.Model || m.Name == c.config.Model {
			return true
		}
	}
	return false
}

// markUnavailable forces the availability cache to false so the generator stops retrying.
func (c *OllamaClient) markUnavailable() {
	c.availMu.Lock()
	defer c.availMu.Unlock()
	unavailable := false
	c.available = &unavailable
	c.availableTime = time.Now()
}

// GenerationHash creates a cache key from model and prompt
func (c *OllamaClient) GenerationHash(prompt string) string {
	h := sha256.New()
	h.Write([]byte("ollama"))
	h.Write([]byte("|"))
	h.Write([]byte(c.config.Model))
	h.Write([]byte("|"))
	h.Write([]byte(prompt))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Generate sends a prompt to Ollama and returns the response.
func (c *OllamaClient) Generate(ctx context.Context, prompt string) (string, error) {
	if !c.IsAvailable() {
		return "", fmt.Errorf("ollama is not available")
	}

	reqBody := ollamaRequest{
		Model:  c.config.Model,
		Prompt: prompt,
		Stream: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 404 means the model isn't pulled — mark unavailable to stop retrying
		if resp.StatusCode == http.StatusNotFound {
			c.markUnavailable()
			return "", fmt.Errorf("ollama model %q not found (run: ollama pull %s)", c.config.Model, c.config.Model)
		}
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var ollamaResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if ollamaResp.Error != "" {
		return "", fmt.Errorf("ollama error: %s", ollamaResp.Error)
	}

	response := strings.TrimSpace(ollamaResp.Response)
	return response, nil
}

// Config returns the current Ollama configuration
func (c *OllamaClient) Config() OllamaConfig {
	return c.config
}
