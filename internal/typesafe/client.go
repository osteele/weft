// Package typesafe is a minimal client for the TypeSafe System One
// evaluation API (https://docs.typesafe.ai/api.md). It covers only the
// question types weft uses: Choice and Noul.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osteele/weft/internal/secrets"
)

const (
	// DefaultBaseURL is the production API origin.
	DefaultBaseURL = "https://api.typesafe.ai"
	// BaseURLEnv overrides DefaultBaseURL.
	BaseURLEnv = "TYPESAFE_BASE_URL"
	// APIKeyEnv holds the API key; it wins over the legacy config file.
	APIKeyEnv = "TYPESAFE_API_KEY"

	systemOnePath = "/v1/systemone"
	// maxErrorBody bounds how much of a non-2xx response body an error
	// carries.
	maxErrorBody = 512
)

// Client calls the System One endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient returns a client for apiKey, honoring TYPESAFE_BASE_URL.
func NewClient(apiKey string) *Client {
	base := strings.TrimSpace(os.Getenv(BaseURLEnv))
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{BaseURL: base, APIKey: apiKey, HTTP: &http.Client{}}
}

// LookupAPIKey returns the TypeSafe API key from TYPESAFE_API_KEY, else from a
// TYPESAFE_API_KEY= line in ~/.config/weft/config. The launchd daemon does not
// inherit the interactive shell's environment, so the file is the durable
// place to keep it. Returns "" when neither supplies a key.
func LookupAPIKey() string {
	if v := strings.TrimSpace(os.Getenv(APIKeyEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	content, err := secrets.ReadCredentialFile(filepath.Join(home, ".config", "weft", "config"))
	if err != nil {
		return ""
	}
	prefix := APIKeyEnv + "="
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// ChoiceQuestion picks one option from Criteria (option -> description).
type ChoiceQuestion struct {
	Instructions any
	Criteria     map[string]any
}

// NoulQuestion is a yes/no question answered with P(yes).
type NoulQuestion struct {
	Instructions any
	// True and False optionally describe what a yes and a no mean.
	True  any
	False any
}

// Request is one evaluation: a state and named questions. Question ids must
// be unique across Choices and Nouls.
type Request struct {
	Model   string
	State   any
	Choices map[string]ChoiceQuestion
	Nouls   map[string]NoulQuestion
}

// ChoiceAnswer is the answer to a ChoiceQuestion.
type ChoiceAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// NoulAnswer is the answer to a NoulQuestion: P(yes) in [0,1].
type NoulAnswer struct {
	Noul float64 `json:"noul"`
}

// Usage is the token accounting for one request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response carries one answer per question, keyed by question id.
type Response struct {
	Model   string
	Choices map[string]ChoiceAnswer
	Nouls   map[string]NoulAnswer
	Usage   Usage
}

// APIError is a non-2xx response. RetryAfter is set from a 429's
// Retry-After header when the header parses.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("typesafe: HTTP %s", e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// RateLimited reports whether the server asked the caller to back off.
func (e *APIError) RateLimited() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type wireRequest struct {
	State     any                     `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type string `json:"type"`
}

type wireResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   Usage                      `json:"usage"`
}

func (r Request) wire() (wireRequest, error) {
	questions := make(map[string]wireQuestion, len(r.Choices)+len(r.Nouls))
	for id, q := range r.Choices {
		questions[id] = wireQuestion{Type: "choice", Instructions: q.Instructions, Criteria: q.Criteria}
	}
	for id, q := range r.Nouls {
		if _, dup := questions[id]; dup {
			return wireRequest{}, fmt.Errorf("typesafe: question id %q used by both a choice and a noul", id)
		}
		wq := wireQuestion{Type: "noul", Instructions: q.Instructions}
		if q.True != nil || q.False != nil {
			criteria := map[string]any{}
			if q.True != nil {
				criteria["true"] = q.True
			}
			if q.False != nil {
				criteria["false"] = q.False
			}
			wq.Criteria = criteria
		}
		questions[id] = wq
	}
	return wireRequest{State: r.State, Model: r.Model, Questions: questions}, nil
}

// SystemOne sends req and returns its answers. Every question in req must be
// answered with its own type; a missing or mistyped answer is an error.
func (c *Client) SystemOne(ctx context.Context, req Request) (Response, error) {
	body, err := req.wire()
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("typesafe: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+systemOnePath, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("typesafe: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("typesafe: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("typesafe: read response (HTTP %s): %w", resp.Status, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: truncate(strings.TrimSpace(string(respBody)), maxErrorBody)}
		if secs, convErr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); convErr == nil && secs > 0 {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		}
		return Response{}, apiErr
	}
	var wire wireResponse
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return Response{}, fmt.Errorf("typesafe: decode response: %w", err)
	}
	return decodeAnswers(req, wire)
}

func decodeAnswers(req Request, wire wireResponse) (Response, error) {
	out := Response{
		Model:   wire.Model,
		Choices: make(map[string]ChoiceAnswer, len(req.Choices)),
		Nouls:   make(map[string]NoulAnswer, len(req.Nouls)),
		Usage:   wire.Usage,
	}
	answer := func(id, wantType string, dst any) error {
		raw, ok := wire.Answers[id]
		if !ok {
			return fmt.Errorf("typesafe: response has no answer for question %q", id)
		}
		var head wireAnswer
		if err := json.Unmarshal(raw, &head); err != nil {
			return fmt.Errorf("typesafe: decode answer %q: %w", id, err)
		}
		if head.Type != wantType {
			return fmt.Errorf("typesafe: answer %q has type %q, want %q", id, head.Type, wantType)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("typesafe: decode answer %q: %w", id, err)
		}
		return nil
	}
	for id := range req.Choices {
		var a ChoiceAnswer
		if err := answer(id, "choice", &a); err != nil {
			return Response{}, err
		}
		if a.Choice == "" {
			return Response{}, fmt.Errorf("typesafe: choice answer %q has no choice", id)
		}
		out.Choices[id] = a
	}
	for id := range req.Nouls {
		var a struct {
			Noul *float64 `json:"noul"`
		}
		if err := answer(id, "noul", &a); err != nil {
			return Response{}, err
		}
		if a.Noul == nil {
			return Response{}, fmt.Errorf("typesafe: noul answer %q has no value", id)
		}
		out.Nouls[id] = NoulAnswer{Noul: *a.Noul}
	}
	return out, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
