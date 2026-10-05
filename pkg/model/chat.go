package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	defaultChatTimeout = 300 * time.Second
	maxChatBytes       = 8 << 20
)

// ChatClient sends single Chat Completions requests. Callers own defaults,
// prompts, response interpretation, and retries.
type ChatClient struct {
	model      core.ModelConfig
	apiKey     []byte
	httpClient *http.Client
	reasoning  map[string]any
	baseURL    url.URL
}

// NewChatClient validates settings and copies the configured credential.
func NewChatClient(model core.ModelConfig, apiKeyLookup func(string) ([]byte, bool)) (*ChatClient, error) {
	if err := ValidateGeneration(model); err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	if model.ContextLength != 0 {
		return nil, errors.New("judge context_length is unsupported: the judge has no context manager")
	}
	reasoning, err := ReasoningBody(model)
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	parsed, err := normalizeChatBaseURL(model.BaseURL)
	if err != nil {
		return nil, err
	}
	key, ok := apiKeyLookup(model.APIKeyEnv)
	if !ok || len(key) == 0 {
		return nil, fmt.Errorf("judge API key environment variable %q is not set", model.APIKeyEnv)
	}
	return &ChatClient{
		model:      model,
		reasoning:  reasoning,
		apiKey:     bytes.Clone(key),
		httpClient: &http.Client{Timeout: defaultChatTimeout},
		baseURL:    *parsed,
	}, nil
}

// Chat returns assistant content without interpreting its benchmark-specific format.
// It sends one request and never retries.
func (client *ChatClient) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	payload := map[string]any{
		"model": client.model.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}
	if client.model.Temperature != nil {
		payload["temperature"] = *client.model.Temperature
	}
	if client.model.MaxTokens != 0 {
		payload["max_tokens"] = client.model.MaxTokens
	}
	for key, value := range client.reasoning {
		payload[key] = value
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode judge request: %w", err)
	}

	endpoint := client.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("judge request: invalid configuration")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+string(client.apiKey))
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("judge request failed: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxChatBytes+1))
	if err != nil {
		return "", fmt.Errorf("read judge response: %w", err)
	}
	if len(responseBody) > maxChatBytes {
		return "", errors.New("judge response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet := truncate([]byte(strings.ReplaceAll(string(responseBody), string(client.apiKey), "[REDACTED]")), 500)
		return "", fmt.Errorf("judge request returned HTTP %d: %s", response.StatusCode, snippet)
	}

	return extractChatContent(responseBody)
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func extractChatContent(body []byte) (string, error) {
	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("parse judge chat-completions response: %w", err)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", errors.New("judge response contained no message content")
	}
	return decoded.Choices[0].Message.Content, nil
}

func normalizeChatBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("judge base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return parsed, nil
}

func truncate(body []byte, max int) string {
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "..."
}
