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

// checkAvailability performs the actual availability check (not thread-safe, called with lock held)
func (c *OllamaClient) checkAvailability() bool {
	// Check if ollama command exists
	if _, err := exec.LookPath("ollama"); err != nil {
		return false
	}

	// Check if ollama server is running by making a simple request
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
	resp.Body.Close()

	return resp.StatusCode == http.StatusOK
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
