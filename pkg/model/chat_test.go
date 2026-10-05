package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestChatClientGenerationSettings(t *testing.T) {
	temperature := 0.4
	for _, tc := range []struct {
		name, provider, effort string
		temperature            *float64
		maxTokens              int
		want                   map[string]any
	}{
		{name: "defaults", provider: "openai", want: map[string]any{}},
		{name: "deepseek off", provider: "deepseek", effort: "off", want: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{name: "deepseek low", provider: "deepseek", effort: "low", want: map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "low"}},
		{name: "openai max with overrides", provider: "openai", effort: "max", temperature: &temperature, maxTokens: 4096, want: map[string]any{"reasoning_effort": "max", "temperature": 0.4, "max_tokens": float64(4096)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["model"] != "test-model" {
					t.Errorf("model = %v", payload["model"])
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fake-key" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect request method, endpoint, or headers")
				}
				wantMessages := []any{map[string]any{"role": "system", "content": "system"}, map[string]any{"role": "user", "content": "user"}}
				if !reflect.DeepEqual(payload["messages"], wantMessages) {
					t.Errorf("messages = %#v", payload["messages"])
				}
				delete(payload, "model")
				delete(payload, "messages")
				if !reflect.DeepEqual(payload, tc.want) {
					t.Errorf("generation fields = %#v, want %#v", payload, tc.want)
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"answer"}}]}`))
			}))
			defer server.Close()
			client, err := NewChatClient(core.ModelConfig{Provider: tc.provider, BaseURL: server.URL + "/v1", APIKeyEnv: "TEST_KEY", Model: "test-model", ReasoningEffort: tc.effort, Temperature: tc.temperature, MaxTokens: tc.maxTokens}, func(string) ([]byte, bool) { return []byte("fake-key"), true })
			if err != nil {
				t.Fatal(err)
			}
			content, err := client.Chat(context.Background(), "system", "user")
			if err != nil || content != "answer" {
				t.Fatalf("chat = %q, %v", content, err)
			}
		})
	}
}

func TestNewChatClientRejectsInvalidURLAndMissingKey(t *testing.T) {
	for _, baseURL := range []string{"", "not-a-url", "ftp://example.com", "https://user:pass@example.com/v1", "https://example.com?key=secret", "https://example.com#fragment"} {
		_, err := NewChatClient(core.ModelConfig{BaseURL: baseURL}, func(string) ([]byte, bool) {
			t.Error("credentials accessed before invalid URL rejected")
			return []byte("fake-key"), true
		})
		if err == nil {
			t.Errorf("accepted invalid base URL %q", baseURL)
		}
	}
	for _, present := range []bool{false, true} {
		_, err := NewChatClient(core.ModelConfig{BaseURL: "https://example.com/v1"}, func(string) ([]byte, bool) { return nil, present })
		if err == nil {
			t.Errorf("accepted empty credentials (present=%v)", present)
		}
	}
}

func TestChatClientResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"malformed JSON", "{", 200, "parse"},
		{"empty body", "", 200, "parse"},
		{"empty choices", `{"choices":[]}`, 200, "no message content"},
		{"empty content", `{"choices":[{"message":{"content":"  "}}]}`, 200, "no message content"},
		{"oversized", strings.Repeat("x", (8<<20)+1), 200, "too large"},
		{"HTTP error", `{"error":"fake-key"}`, 500, "HTTP 500"},
		{"key straddles truncation", strings.Repeat("x", 496) + "fake-key", 400, "HTTP 400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewChatClient(core.ModelConfig{BaseURL: server.URL}, func(string) ([]byte, bool) { return []byte("fake-key"), true })
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Chat(context.Background(), "system", "user")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Chat error = %v, want %q", err, tc.want)
			}
			if calls.Load() != 1 {
				t.Errorf("Chat sent %d requests, want one without retries", calls.Load())
			}
			if strings.Contains(err.Error(), "fake") {
				t.Fatalf("error leaked credential bytes: %v", err)
			}
		})
	}
}

func TestChatClientCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewChatClient(core.ModelConfig{BaseURL: server.URL}, func(string) ([]byte, bool) { return []byte("fake-key"), true })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-started; cancel() }()
	_, err = client.Chat(ctx, "system", "user")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want cancellation", err)
	}
}
