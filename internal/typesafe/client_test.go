package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSystemOneSendsTypedQuestionsAndParsesAnswers(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		_, _ = io.WriteString(w, `{
			"model": "jev-1.13.0",
			"answers": {
				"category": {"type": "choice", "choice": "gpu_oom", "probabilities": {"gpu_oom": 0.9, "other": 0.1}, "confidence": 0.8},
				"supported": {"type": "noul", "noul": 0.95}
			},
			"usage": {"input_tokens": 321, "output_tokens": 12}
		}`)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, APIKey: "sk-test", HTTP: srv.Client()}
	resp, err := client.SystemOne(context.Background(), Request{
		Model: "jev-1.13.0",
		State: map[string]any{"command": "python train.py", "exit_code": 1},
		Choices: map[string]ChoiceQuestion{
			"category": {Instructions: "Why did it fail?", Criteria: map[string]any{
				"gpu_oom": map[string]any{"what": "GPU OOM"},
				"other":   "anything else",
			}},
		},
		Nouls: map[string]NoulQuestion{
			"supported": {Instructions: "Is it OOM?", True: "yes", False: "no"},
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}

	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotBody["model"] != "jev-1.13.0" {
		t.Errorf("model = %v", gotBody["model"])
	}
	state, _ := gotBody["state"].(map[string]any)
	if state["command"] != "python train.py" || state["exit_code"] != float64(1) {
		t.Errorf("state = %v, want the object passed in", gotBody["state"])
	}
	questions, _ := gotBody["questions"].(map[string]any)
	category, _ := questions["category"].(map[string]any)
	if category["type"] != "choice" || category["instructions"] != "Why did it fail?" {
		t.Errorf("category question = %v, want a choice with its instructions", category)
	}
	criteria, _ := category["criteria"].(map[string]any)
	if gpu, _ := criteria["gpu_oom"].(map[string]any); gpu["what"] != "GPU OOM" || criteria["other"] != "anything else" {
		t.Errorf("choice criteria = %v, want object and string options", criteria)
	}
	supported, _ := questions["supported"].(map[string]any)
	noulCriteria, _ := supported["criteria"].(map[string]any)
	if supported["type"] != "noul" || noulCriteria["true"] != "yes" || noulCriteria["false"] != "no" {
		t.Errorf("noul question = %v, want type noul with true/false criteria", supported)
	}

	choice := resp.Choices["category"]
	if choice.Choice != "gpu_oom" || choice.Confidence != 0.8 || choice.Probabilities["gpu_oom"] != 0.9 {
		t.Errorf("choice answer = %+v", choice)
	}
	if got := resp.Nouls["supported"].Noul; got != 0.95 {
		t.Errorf("noul answer = %v, want 0.95", got)
	}
	if resp.Model != "jev-1.13.0" || resp.Usage.InputTokens != 321 {
		t.Errorf("model/usage = %q/%+v", resp.Model, resp.Usage)
	}
}

func TestSystemOneNon2xxSurfacesStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		retryAfter    string
		wantRateLimit bool
		wantRetry     time.Duration
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "rate limited", status: http.StatusTooManyRequests, retryAfter: "7", wantRateLimit: true, wantRetry: 7 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"upstream said no"}`)
			}))
			defer srv.Close()

			client := &Client{BaseURL: srv.URL, APIKey: "sk-secret-key", HTTP: srv.Client()}
			_, err := client.SystemOne(context.Background(), Request{
				Model:   "jev-1.13.0",
				State:   "log",
				Choices: map[string]ChoiceQuestion{"c": {Instructions: "?", Criteria: map[string]any{"a": "A"}}},
			})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			msg := err.Error()
			if !strings.Contains(msg, http.StatusText(tc.status)) || !strings.Contains(msg, "upstream said no") {
				t.Errorf("error %q should name the status and carry the body", msg)
			}
			if strings.Contains(msg, "sk-secret-key") {
				t.Errorf("error %q leaks the API key", msg)
			}
			if apiErr.RateLimited() != tc.wantRateLimit || apiErr.RetryAfter != tc.wantRetry {
				t.Errorf("RateLimited=%v RetryAfter=%v, want %v %v", apiErr.RateLimited(), apiErr.RetryAfter, tc.wantRateLimit, tc.wantRetry)
			}
		})
	}
}

// An answer the server omitted is unknown, not a zero probability.
func TestSystemOneRejectsMissingAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"c":{"type":"choice","choice":"a","probabilities":{"a":1},"confidence":1}},"usage":{}}`)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	_, err := client.SystemOne(context.Background(), Request{
		Model:   "jev-1.13.0",
		State:   "log",
		Choices: map[string]ChoiceQuestion{"c": {Instructions: "?", Criteria: map[string]any{"a": "A"}}},
		Nouls:   map[string]NoulQuestion{"n": {Instructions: "?"}},
	})
	if err == nil || !strings.Contains(err.Error(), `"n"`) {
		t.Fatalf("err = %v, want an error naming the unanswered noul", err)
	}
}

func TestLookupAPIKeyEnvBeatsCredentialFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "weft", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=other\nTYPESAFE_API_KEY= file-key \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(APIKeyEnv, "env-key")
	if got := LookupAPIKey(); got != "env-key" {
		t.Errorf("with env set: LookupAPIKey = %q, want env-key", got)
	}
	t.Setenv(APIKeyEnv, "")
	if got := LookupAPIKey(); got != "file-key" {
		t.Errorf("with env unset: LookupAPIKey = %q, want file-key", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := LookupAPIKey(); got != "" {
		t.Errorf("with neither: LookupAPIKey = %q, want empty", got)
	}
}
