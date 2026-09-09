package sweatlas

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
	defaultJudgeTimeout = 300 * time.Second
	maxJudgeBytes       = 8 << 20
)

// judgeClient is a generic OpenAI-compatible chat-completions client, ported
// from Deep Research Bench's judgeClient
// (pkg/benchmark/deepresearchbench/judge.go) so sweatlas's rubric judge call
// runs host-side instead of inside the sandbox: evaluate_answer.py's own
// openai.OpenAI(...).chat.completions.create(...) call is replaced by this
// client's chat method.
type judgeClient struct {
	model      core.ModelConfig
	apiKey     []byte
	httpClient *http.Client
	baseURL    url.URL
}

func newJudgeClient(model core.ModelConfig, apiKeyLookup func(string) ([]byte, bool)) (*judgeClient, error) {
	parsed, err := normalizeJudgeBaseURL(model.BaseURL)
	if err != nil {
		return nil, err
	}
	key, ok := apiKeyLookup(model.APIKeyEnv)
	if !ok || len(key) == 0 {
		return nil, fmt.Errorf("judge API key environment variable %q is not set", model.APIKeyEnv)
	}
	return &judgeClient{
		model:      model,
		apiKey:     bytes.Clone(key),
		httpClient: &http.Client{Timeout: defaultJudgeTimeout},
		baseURL:    *parsed,
	}, nil
}

// chatter is the minimal interface rubric scoring depends on for LLM calls,
// so tests can substitute a fake without an HTTP server.
type chatter interface {
	chat(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

var _ chatter = (*judgeClient)(nil)

// chat sends one chat-completions request with the given system and user
// prompts and returns the raw assistant message content string. No
// response_format is set, matching evaluate_answer.py, which relies on
// prompt instructions alone to get back parsable JSON.
func (client *judgeClient) chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	payload := map[string]any{
		"model": client.model.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
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

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxJudgeBytes+1))
	if err != nil {
		return "", fmt.Errorf("read judge response: %w", err)
	}
	if len(responseBody) > maxJudgeBytes {
		return "", errors.New("judge response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet := strings.ReplaceAll(truncate(responseBody, 500), string(client.apiKey), "[REDACTED]")
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

func normalizeJudgeBaseURL(raw string) (*url.URL, error) {
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
