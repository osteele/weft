package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	// DefaultModel is the default ollama model to use
	DefaultModel = "llama3.2"

	// DefaultPromptTemplate is the prompt template for generating descriptions
	DefaultPromptTemplate = `Describe what this shell command does in under 10 words.
Focus on the specific task, model names, or data being processed.
Avoid generic phrases like "run script" or "execute command".
Output ONLY the description, nothing else.

Command: %s

Description:`

	// OllamaAPIURL is the default ollama API endpoint
	OllamaAPIURL = "http://localhost:11434/api/generate"

	// RequestTimeout is the timeout for ollama API requests
	RequestTimeout = 30 * time.Second
)

// Config holds the configuration for LLM description generation
type Config struct {
	Model          string
	PromptTemplate string
	APIURL         string
}

// DefaultConfig returns the default configuration
func DefaultConfig() Config {
	return Config{
		Model:          DefaultModel,
		PromptTemplate: DefaultPromptTemplate,
		APIURL:         OllamaAPIURL,
	}
}

// ConfigWithModel returns a config with the specified model
func ConfigWithModel(model string) Config {
	cfg := DefaultConfig()
	if model != "" {
		cfg.Model = model
	}
	return cfg
}

// GenerationHash computes a hash of the model, prompt template, and command
// This allows detecting when regeneration is needed due to config changes
func (c Config) GenerationHash(command string) string {
	h := sha256.New()
	h.Write([]byte(c.Model))
	h.Write([]byte("|"))
	h.Write([]byte(c.PromptTemplate))
	h.Write([]byte("|"))
	h.Write([]byte(command))
	return hex.EncodeToString(h.Sum(nil))[:16] // Use first 16 chars
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

// Client provides access to local LLM services
type Client struct {
	config     Config
	httpClient *http.Client
	available  *bool // Cached availability check
}

// NewClient creates a new LLM client with the given configuration
func NewClient(config Config) *Client {
	return &Client{
		config: config,
		httpClient: &http.Client{
			Timeout: RequestTimeout,
		},
	}
}

// NewDefaultClient creates a new LLM client with default configuration
func NewDefaultClient() *Client {
	return NewClient(DefaultConfig())
}

// IsAvailable checks if ollama is installed and running
func (c *Client) IsAvailable() bool {
	if c.available != nil {
		return *c.available
	}

	// Check if ollama command exists
	_, err := exec.LookPath("ollama")
	if err != nil {
		available := false
		c.available = &available
		return false
	}

	// Check if ollama server is running by making a simple request
	resp, err := c.httpClient.Get("http://localhost:11434/api/tags")
	if err != nil {
		available := false
		c.available = &available
		return false
	}
	resp.Body.Close()

	available := resp.StatusCode == http.StatusOK
	c.available = &available
	return available
}

// GenerateDescription generates a description for a shell command
func (c *Client) GenerateDescription(command string) (description string, hash string, err error) {
	if !c.IsAvailable() {
		return "", "", fmt.Errorf("ollama is not available")
	}

	prompt := fmt.Sprintf(c.config.PromptTemplate, command)

	reqBody := ollamaRequest{
		Model:  c.config.Model,
		Prompt: prompt,
		Stream: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.httpClient.Post(c.config.APIURL, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return "", "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var ollamaResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}

	if ollamaResp.Error != "" {
		return "", "", fmt.Errorf("ollama error: %s", ollamaResp.Error)
	}

	// Clean up the response - remove newlines and extra whitespace
	description = strings.TrimSpace(ollamaResp.Response)
	description = strings.ReplaceAll(description, "\n", " ")
	description = strings.Join(strings.Fields(description), " ")

	// Truncate if too long (max 100 chars for display)
	if len(description) > 100 {
		description = description[:97] + "..."
	}

	hash = c.config.GenerationHash(command)

	return description, hash, nil
}

// Config returns the current configuration
func (c *Client) Config() Config {
	return c.config
}
