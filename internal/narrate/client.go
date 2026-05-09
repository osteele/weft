package narrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
	openRouterAPIURL    = "https://openrouter.ai/api/v1/chat/completions"
	defaultTimeout      = 60 * time.Second
)

const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
)

// ClientConfig configures the narrator client.
type ClientConfig struct {
	Provider        string
	APIKey          string
	Model           string
	MaxOutputTokens int
	HTTPClient      *http.Client
}

// Client wraps LLM API calls for narration. It deliberately keeps a minimal
// surface: narration and compaction, both returning normalized usage stats.
type Client struct {
	cfg  ClientConfig
	http *http.Client
}

// NewClient builds a client from cfg.
func NewClient(cfg ClientConfig) *Client {
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 600
	}
	cfg.Provider = normalizeProvider(cfg.Provider)
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{cfg: cfg, http: cfg.HTTPClient}
}

// IsAvailable reports whether the client has an API key.
func (c *Client) IsAvailable() bool {
	return strings.TrimSpace(c.cfg.APIKey) != ""
}

// LookupAPIKey returns the Anthropic API key from the environment or legacy
// KEY=VALUE config file. It is kept for older callers; new code should call
// LookupProviderAPIKey with a provider and structured-config fallback.
func LookupAPIKey() string {
	return LookupProviderAPIKey(ProviderAnthropic, "")
}

// LookupProviderAPIKey returns the configured key for provider. Environment
// variables win over structured config, which wins over the legacy
// ~/.config/weft/config KEY=VALUE file.
func LookupProviderAPIKey(provider, configured string) string {
	provider = normalizeProvider(provider)
	for _, key := range providerEnvKeys(provider) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	return lookupLegacyAPIKey(providerLegacyKeys(provider)...)
}

func providerEnvKeys(provider string) []string {
	switch normalizeProvider(provider) {
	case ProviderOpenRouter:
		return []string{"OPENROUTER_API_KEY", "CLAUDE_OPENROUTER_API_KEY", "CLAUDEM_OPENROUTER_API_KEY"}
	default:
		return []string{"ANTHROPIC_API_KEY"}
	}
}

func providerLegacyKeys(provider string) []string {
	switch normalizeProvider(provider) {
	case ProviderOpenRouter:
		return []string{"OPENROUTER_API_KEY", "CLAUDE_OPENROUTER_API_KEY", "CLAUDEM_OPENROUTER_API_KEY"}
	default:
		return []string{"ANTHROPIC_API_KEY"}
	}
}

func lookupLegacyAPIKey(keys ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	content, err := os.ReadFile(filepath.Join(home, ".config", "weft", "config"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		for _, key := range keys {
			prefix := key + "="
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", ProviderAnthropic:
		return ProviderAnthropic
	case ProviderOpenRouter:
		return ProviderOpenRouter
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

// Report is the structured output we extract from the model's tool call.
type Report struct {
	Narration  string `json:"narration"`
	StateRecap string `json:"state_recap"`
}

// Usage holds per-call cache statistics. Logged for cost regression detection.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// Tick payload passed to Narrate. Keeping these as separate fields rather
// than a single blob lets us place the cache breakpoint precisely.
type Tick struct {
	PriorRecap      string // accumulated <prior_state_recap> body (may be empty)
	CurrentSnapshot string // structured current-state JSON
	Delta           string // structured changes JSON
	Now             time.Time
	Since           time.Time // timestamp of the previous snapshot (basis for the changes block)
}

// Narrate calls Claude once and returns the structured report and usage stats.
func (c *Client) Narrate(ctx context.Context, tick Tick) (*Report, Usage, error) {
	if !c.IsAvailable() {
		return nil, Usage{}, fmt.Errorf("%s API key not configured", c.cfg.Provider)
	}
	if c.cfg.Provider == ProviderOpenRouter {
		return c.narrateOpenRouter(ctx, tick)
	}
	return c.narrateAnthropic(ctx, tick)
}

func narrationBlocks(tick Tick) ([]map[string]any, []map[string]any) {
	// systemText is stable across calls and marked cache_control so every
	// call after the first can read from cache.
	systemText := systemPrompt + "\n\n" + glossary
	systemBlocks := []map[string]any{
		{
			"type":          "text",
			"text":          systemText,
			"cache_control": map[string]string{"type": "ephemeral"},
		},
	}

	// The prior-recap block grows append-only inside the user message and
	// carries its own cache breakpoint, so the volatile snapshot+delta
	// after it is the only uncached tail.
	userBlocks := []map[string]any{}
	if tick.PriorRecap != "" {
		userBlocks = append(userBlocks, map[string]any{
			"type":          "text",
			"text":          "<prior_state_recap>\n" + tick.PriorRecap + "\n</prior_state_recap>\n\n",
			"cache_control": map[string]string{"type": "ephemeral"},
		})
	}
	var tail strings.Builder
	tail.WriteString("CURRENT_STATE (snapshot at ")
	tail.WriteString(tick.Now.Local().Format("15:04:05"))
	tail.WriteString("):\n")
	tail.WriteString(tick.CurrentSnapshot)
	tail.WriteString("\n\nCHANGES (transitions since ")
	if !tick.Since.IsZero() {
		tail.WriteString(tick.Since.Local().Format("15:04:05"))
	} else {
		tail.WriteString("the previous snapshot")
	}
	tail.WriteString("):\n")
	tail.WriteString(tick.Delta)
	tail.WriteString("\n\nCall the report tool now.")
	userBlocks = append(userBlocks, map[string]any{
		"type": "text",
		"text": tail.String(),
	})
	return systemBlocks, userBlocks
}

// narrateAnthropic calls Anthropic Messages once and returns the structured
// report and usage stats.
func (c *Client) narrateAnthropic(ctx context.Context, tick Tick) (*Report, Usage, error) {
	systemBlocks, userBlocks := narrationBlocks(tick)

	body := map[string]any{
		"model":      c.cfg.Model,
		"max_tokens": c.cfg.MaxOutputTokens,
		"system":     systemBlocks,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": userBlocks,
			},
		},
		"tools": []map[string]any{
			{
				"name":         reportToolName,
				"description":  "Emit a narration paragraph for the operator and a terse state recap for the next tick.",
				"input_schema": reportToolSchema,
			},
		},
		"tool_choice": map[string]any{"type": "tool", "name": reportToolName},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(raw))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("anthropic API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return nil, Usage{}, fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}

	var parsed struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, Usage{}, fmt.Errorf("decode response: %w", err)
	}

	usage := Usage{
		InputTokens:         parsed.Usage.InputTokens,
		OutputTokens:        parsed.Usage.OutputTokens,
		CacheCreationTokens: parsed.Usage.CacheCreationInputTokens,
		CacheReadTokens:     parsed.Usage.CacheReadInputTokens,
	}

	for _, block := range parsed.Content {
		if block.Type == "tool_use" && block.Name == reportToolName {
			var rep Report
			if err := json.Unmarshal(block.Input, &rep); err != nil {
				return nil, usage, fmt.Errorf("decode tool input: %w", err)
			}
			return &rep, usage, nil
		}
	}
	return nil, usage, fmt.Errorf("model response did not include a %s tool call", reportToolName)
}

func (c *Client) narrateOpenRouter(ctx context.Context, tick Tick) (*Report, Usage, error) {
	systemBlocks, userBlocks := narrationBlocks(tick)
	body := map[string]any{
		"model":      c.cfg.Model,
		"max_tokens": c.cfg.MaxOutputTokens,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": systemBlocks,
			},
			{
				"role":    "user",
				"content": userBlocks,
			},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        reportToolName,
					"description": "Emit a narration paragraph for the operator and a terse state recap for the next tick.",
					"parameters":  reportToolSchema,
				},
			},
		},
		"tool_choice": map[string]any{
			"type": "function",
			"function": map[string]string{
				"name": reportToolName,
			},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", openRouterAPIURL, bytes.NewReader(raw))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("openrouter API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return nil, Usage{}, fmt.Errorf("openrouter API status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}

	var parsed openRouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, Usage{}, fmt.Errorf("decode response: %w", err)
	}
	usage := parsed.usage()
	for _, choice := range parsed.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.Function.Name != reportToolName {
				continue
			}
			rep, err := c.decodeOpenRouterReport(call.Function.Arguments)
			if err != nil {
				return nil, usage, err
			}
			return rep, usage, nil
		}
	}
	return nil, usage, fmt.Errorf("model response did not include a %s tool call", reportToolName)
}

func (c *Client) decodeOpenRouterReport(arguments string) (*Report, error) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return &Report{}, nil
	}
	var rep Report
	if err := json.Unmarshal([]byte(arguments), &rep); err != nil {
		if strings.Contains(err.Error(), "unexpected end of JSON input") {
			return &Report{}, nil
		}
		return nil, fmt.Errorf("openrouter model %s returned invalid %s tool arguments: %w", c.cfg.Model, reportToolName, err)
	}
	return &rep, nil
}

// CompactRecaps is a one-shot call that summarizes a chain of prior recaps
// into a fresh "story so far" block. Used when the recap budget is exceeded.
func (c *Client) CompactRecaps(ctx context.Context, priorRecap string) (string, Usage, error) {
	if !c.IsAvailable() {
		return "", Usage{}, fmt.Errorf("%s API key not configured", c.cfg.Provider)
	}
	if c.cfg.Provider == ProviderOpenRouter {
		return c.compactRecapsOpenRouter(ctx, priorRecap)
	}
	return c.compactRecapsAnthropic(ctx, priorRecap)
}

func (c *Client) compactRecapsAnthropic(ctx context.Context, priorRecap string) (string, Usage, error) {
	system := "You compact a chain of weft operations recaps into a single fresh recap. Output one terse bulleted recap that preserves all open threads and current state. Do not invent. No prose."
	user := "<prior_state_recap>\n" + priorRecap + "\n</prior_state_recap>\n\nReturn only the compacted recap text."

	body := map[string]any{
		"model":      c.cfg.Model,
		"max_tokens": 800,
		"system":     []map[string]any{{"type": "text", "text": system}},
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": user}}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(raw))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("anthropic API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return "", Usage{}, fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", Usage{}, err
	}
	usage := Usage{
		InputTokens:         parsed.Usage.InputTokens,
		OutputTokens:        parsed.Usage.OutputTokens,
		CacheCreationTokens: parsed.Usage.CacheCreationInputTokens,
		CacheReadTokens:     parsed.Usage.CacheReadInputTokens,
	}
	for _, block := range parsed.Content {
		if block.Type == "text" {
			return strings.TrimSpace(block.Text), usage, nil
		}
	}
	return "", usage, fmt.Errorf("compaction response was empty")
}

func (c *Client) compactRecapsOpenRouter(ctx context.Context, priorRecap string) (string, Usage, error) {
	system := "You compact a chain of weft operations recaps into a single fresh recap. Output one terse bulleted recap that preserves all open threads and current state. Do not invent. No prose."
	user := "<prior_state_recap>\n" + priorRecap + "\n</prior_state_recap>\n\nReturn only the compacted recap text."
	body := map[string]any{
		"model":      c.cfg.Model,
		"max_tokens": 800,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", openRouterAPIURL, bytes.NewReader(raw))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return "", Usage{}, fmt.Errorf("openrouter API status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	var parsed openRouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", Usage{}, err
	}
	usage := parsed.usage()
	for _, choice := range parsed.Choices {
		if text := strings.TrimSpace(choice.Message.Content); text != "" {
			return text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("compaction response was empty")
}

type openRouterResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens              int `json:"cached_tokens"`
			CacheWriteTokens          int `json:"cache_write_tokens"`
			CacheCreationInputTokens  int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens      int `json:"cache_read_input_tokens"`
			CacheCreationOutputTokens int `json:"cache_creation_output_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (r openRouterResponse) usage() Usage {
	return Usage{
		InputTokens:         r.Usage.PromptTokens,
		OutputTokens:        r.Usage.CompletionTokens,
		CacheCreationTokens: firstNonZero(r.Usage.PromptDetails.CacheWriteTokens, r.Usage.PromptDetails.CacheCreationInputTokens),
		CacheReadTokens:     firstNonZero(r.Usage.PromptDetails.CachedTokens, r.Usage.PromptDetails.CacheReadInputTokens),
	}
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
