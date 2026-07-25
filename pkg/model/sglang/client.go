// Package sglang implements the small OpenAI-compatible endpoint surface ARIES uses.
package sglang

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
	"sync"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 1 << 20
)

const (
	CategoryTransport        = "transport"
	CategoryResponseRead     = "response_read"
	CategoryResponseTooLarge = "response_too_large"
	CategoryHTTPStatus       = "http_status"
	CategoryMalformed        = "malformed"
)

// Failure exposes only a bounded category and status; provider bodies and transport details stay private.
type Failure struct {
	Operation  string
	Category   string
	StatusCode int
	cause      error
}

func (failure *Failure) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("sglang %s: unexpected HTTP status %d", failure.Operation, failure.StatusCode)
	}
	label := "request failed"
	switch failure.Category {
	case CategoryTransport:
		label = "transport error"
	case CategoryResponseRead:
		label = "response read error"
	case CategoryResponseTooLarge:
		label = "response too large"
	case CategoryMalformed:
		label = "malformed response"
	}
	return fmt.Sprintf("sglang %s: %s", failure.Operation, label)
}

func (failure *Failure) Unwrap() error { return failure.cause }

type Client struct {
	mu      sync.Mutex
	active  sync.WaitGroup
	baseURL url.URL
	apiKey  []byte
	http    *http.Client
	closed  bool
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model           string
	Messages        []Message
	Temperature     float64
	MaxTokens       int
	Seed            *int64
	DisableThinking bool
}

type ChatResponse struct {
	Content          string
	Reasoning        string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
}

func New(baseURL string, apiKey []byte, httpClient *http.Client) (*Client, error) {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(normalized)
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	} else {
		clone := *httpClient
		httpClient = &clone
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: *parsed, apiKey: bytes.Clone(apiKey), http: httpClient}, nil
}

// NormalizeBaseURL validates the exact OpenAI-compatible API root accepted by ARIES.
func NormalizeBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return "", errors.New("sglang base URL must be an absolute HTTP(S) URL without credentials, escaped path, query, or fragment")
	}
	if parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return "", errors.New("sglang base URL path must be exactly /v1")
	}
	parsed.Path = "/v1"
	return parsed.String(), nil
}

// Close idempotently clears the client's owned credential clone and rejects future requests.
func (client *Client) Close() {
	client.mu.Lock()
	client.closed = true
	client.mu.Unlock()
	client.active.Wait()
	client.mu.Lock()
	clear(client.apiKey)
	client.apiKey = nil
	client.mu.Unlock()
}

func (client *Client) Models(ctx context.Context) ([]string, error) {
	authorization, err := client.begin()
	if err != nil {
		return nil, err
	}
	defer client.active.Done()
	request, err := client.request(ctx, http.MethodGet, "/models", nil, authorization)
	if err != nil {
		return nil, err
	}
	body, err := client.do(request, "models")
	if err != nil {
		return nil, err
	}
	defer clear(body)
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil {
		return nil, &Failure{Operation: "models", Category: CategoryMalformed}
	}
	models := make([]string, 0, len(response.Data))
	for _, item := range response.Data {
		if strings.TrimSpace(item.ID) == "" {
			return nil, &Failure{Operation: "models", Category: CategoryMalformed}
		}
		models = append(models, item.ID)
	}
	return models, nil
}

func (client *Client) Chat(ctx context.Context, input ChatRequest) (ChatResponse, error) {
	authorization, err := client.begin()
	if err != nil {
		return ChatResponse{}, err
	}
	defer client.active.Done()
	if strings.TrimSpace(input.Model) == "" {
		return ChatResponse{}, errors.New("sglang chat: model is required")
	}
	payload := struct {
		Model              string          `json:"model"`
		Messages           []Message       `json:"messages"`
		Temperature        float64         `json:"temperature"`
		MaxTokens          int             `json:"max_tokens"`
		Stream             bool            `json:"stream"`
		Seed               *int64          `json:"seed,omitempty"`
		ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs,omitempty"`
	}{Model: input.Model, Messages: input.Messages, Temperature: input.Temperature, MaxTokens: input.MaxTokens, Seed: input.Seed}
	if input.DisableThinking {
		payload.ChatTemplateKwargs = map[string]bool{"enable_thinking": false}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return ChatResponse{}, errors.New("sglang chat: encode request")
	}
	request, err := client.request(ctx, http.MethodPost, "/chat/completions", &encoded, authorization)
	if err != nil {
		return ChatResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	body, err := client.do(request, "chat")
	if err != nil {
		return ChatResponse{}, err
	}
	defer clear(body)
	var response struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return ChatResponse{}, errors.New("sglang chat: malformed response")
	}
	if len(response.Choices) == 0 {
		return ChatResponse{}, errors.New("sglang chat: empty choices")
	}
	choice := response.Choices[0]
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}
	content := choice.Message.Content
	if content == "" {
		content = reasoning
	}
	if content == "" {
		return ChatResponse{}, errors.New("sglang chat: empty content")
	}
	return ChatResponse{Content: content, Reasoning: reasoning, FinishReason: choice.FinishReason, PromptTokens: response.Usage.PromptTokens, CompletionTokens: response.Usage.CompletionTokens}, nil
}

func (client *Client) begin() (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return "", errors.New("sglang client is closed")
	}
	client.active.Add(1)
	if len(client.apiKey) == 0 {
		return "", nil
	}
	return "Bearer " + string(client.apiKey), nil
}

func (client *Client) request(ctx context.Context, method, route string, body io.Reader, authorization string) (*http.Request, error) {
	endpoint := client.baseURL
	endpoint.Path += route
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, errors.New("sglang request: invalid configuration")
	}
	request.Header.Set("Accept", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return request, nil
}

func (client *Client) do(request *http.Request, operation string) ([]byte, error) {
	response, err := client.http.Do(request)
	request.Header.Del("Authorization")
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if request.Context().Err() != nil {
			return nil, &Failure{Operation: operation, Category: CategoryTransport, cause: request.Context().Err()}
		}
		return nil, &Failure{Operation: operation, Category: CategoryTransport}
	}
	if response == nil {
		return nil, &Failure{Operation: operation, Category: CategoryTransport}
	}
	if response.Body == nil {
		return nil, &Failure{Operation: operation, Category: CategoryResponseRead}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		clear(body)
		return nil, &Failure{Operation: operation, Category: CategoryResponseRead}
	}
	if len(body) > maxResponseBytes {
		clear(body)
		return nil, &Failure{Operation: operation, Category: CategoryResponseTooLarge}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		clear(body)
		return nil, &Failure{Operation: operation, Category: CategoryHTTPStatus, StatusCode: response.StatusCode}
	}
	return body, nil
}
