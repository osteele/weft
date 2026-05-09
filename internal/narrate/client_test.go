package narrate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type captureTransport struct {
	req  *http.Request
	body []byte
	resp string
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.req = req
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(t.resp)),
		Header:     make(http.Header),
	}, nil
}

func testTick() Tick {
	return Tick{
		PriorRecap:      "previous state",
		CurrentSnapshot: `{"jobs":[]}`,
		Delta:           `{"jobs_changed":[]}`,
		Now:             time.Unix(1710000000, 0),
		Since:           time.Unix(1709999900, 0),
	}
}

func TestNarrateAnthropicRequestUsesMessagesAPIAndCacheBreakpoints(t *testing.T) {
	rt := &captureTransport{resp: `{
		"content":[{"type":"tool_use","name":"report","input":{"narration":"ok","state_recap":"recap"}}],
		"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":10,"cache_read_input_tokens":80}
	}`}
	client := NewClient(ClientConfig{
		Provider:   ProviderAnthropic,
		APIKey:     "key",
		Model:      "claude-sonnet-4-20250514",
		HTTPClient: &http.Client{Transport: rt},
	})

	report, usage, err := client.Narrate(context.Background(), testTick())
	if err != nil {
		t.Fatal(err)
	}
	if report.Narration != "ok" || report.StateRecap != "recap" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if usage.CacheCreationTokens != 10 || usage.CacheReadTokens != 80 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if rt.req.URL.String() != anthropicAPIURL {
		t.Fatalf("url = %s", rt.req.URL.String())
	}
	if rt.req.Header.Get("x-api-key") != "key" {
		t.Fatal("missing Anthropic x-api-key header")
	}
	if rt.req.Header.Get("Authorization") != "" {
		t.Fatal("Anthropic request should not use Authorization header")
	}

	var body map[string]any
	if err := json.Unmarshal(rt.body, &body); err != nil {
		t.Fatal(err)
	}
	system := body["system"].([]any)
	first := system[0].(map[string]any)
	if _, ok := first["cache_control"]; !ok {
		t.Fatalf("system cache_control missing: %#v", first)
	}
}

func TestNarrateOpenRouterRequestUsesChatCompletionsAndCacheBreakpoints(t *testing.T) {
	rt := &captureTransport{resp: `{
		"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"report","arguments":"{\"narration\":\"ok\",\"state_recap\":\"recap\"}"}}]}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":10}}
	}`}
	client := NewClient(ClientConfig{
		Provider:   ProviderOpenRouter,
		APIKey:     "key",
		Model:      "anthropic/claude-sonnet-4.6",
		HTTPClient: &http.Client{Transport: rt},
	})

	report, usage, err := client.Narrate(context.Background(), testTick())
	if err != nil {
		t.Fatal(err)
	}
	if report.Narration != "ok" || report.StateRecap != "recap" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if usage.CacheCreationTokens != 10 || usage.CacheReadTokens != 80 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if rt.req.URL.String() != openRouterAPIURL {
		t.Fatalf("url = %s", rt.req.URL.String())
	}
	if rt.req.Header.Get("Authorization") != "Bearer key" {
		t.Fatal("missing OpenRouter bearer token")
	}
	if rt.req.Header.Get("x-api-key") != "" {
		t.Fatal("OpenRouter request should not use x-api-key")
	}

	var body map[string]any
	if err := json.Unmarshal(rt.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("model = %v", body["model"])
	}
	messages := body["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" {
		t.Fatalf("first role = %v", system["role"])
	}
	content := system["content"].([]any)
	firstBlock := content[0].(map[string]any)
	if _, ok := firstBlock["cache_control"]; !ok {
		t.Fatalf("system cache_control missing: %#v", firstBlock)
	}
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool type = %v", tool["type"])
	}
}

func TestNewClientDefaultMaxOutputTokensLeavesRoomForToolJSON(t *testing.T) {
	client := NewClient(ClientConfig{APIKey: "key"})
	if client.cfg.MaxOutputTokens != 1600 {
		t.Fatalf("MaxOutputTokens = %d, want 1600", client.cfg.MaxOutputTokens)
	}
}

func TestLookupProviderAPIKeyPrefersOpenRouterEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "from-env")
	if got := LookupProviderAPIKey(ProviderOpenRouter, "from-config"); got != "from-env" {
		t.Fatalf("key = %q, want env", got)
	}
}

func TestNarrateOpenRouterErrorsOnEmptyToolArguments(t *testing.T) {
	rt := &captureTransport{resp: `{
		"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"report","arguments":""}}]}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20}
	}`}
	client := NewClient(ClientConfig{
		Provider:   ProviderOpenRouter,
		APIKey:     "key",
		Model:      "anthropic/claude-sonnet-4.6",
		HTTPClient: &http.Client{Transport: rt},
	})

	_, _, err := client.Narrate(context.Background(), testTick())
	if err == nil {
		t.Fatal("Narrate succeeded, want empty tool arguments error")
	}
	if !strings.Contains(err.Error(), "empty report tool arguments") {
		t.Fatalf("error = %v", err)
	}
}

func TestNarrateOpenRouterErrorsOnTruncatedToolArguments(t *testing.T) {
	rt := &captureTransport{resp: `{
		"choices":[{"finish_reason":"length","native_finish_reason":"max_tokens","message":{"tool_calls":[{"type":"function","function":{"name":"report","arguments":"{"}}]}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20}
	}`}
	client := NewClient(ClientConfig{
		Provider:   ProviderOpenRouter,
		APIKey:     "key",
		Model:      "anthropic/claude-sonnet-4.6",
		HTTPClient: &http.Client{Transport: rt},
	})

	_, _, err := client.Narrate(context.Background(), testTick())
	if err == nil {
		t.Fatal("Narrate succeeded, want invalid tool arguments error")
	}
	if !strings.Contains(err.Error(), "invalid report tool arguments") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), `finish_reason="length"`) || !strings.Contains(err.Error(), `native_finish_reason="max_tokens"`) {
		t.Fatalf("error missing finish metadata: %v", err)
	}
}
