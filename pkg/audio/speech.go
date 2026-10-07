package audio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSpeechBaseURL = "https://api.openai.com/v1"
	DefaultSpeechModel   = "gpt-4o-mini-tts"
	DefaultSpeechVoice   = "alloy"
	DefaultSpeechFormat  = "wav"
	DefaultSpeechTimeout = 120 * time.Second
	maxSpeechBytes       = 64 << 20
	speechMaxAttempts    = 3
	speechRetryBaseDelay = 250 * time.Millisecond
	speechRetryMaxDelay  = 5 * time.Second
)

type SpeechClientOptions struct {
	BaseURL    string
	APIKey     []byte
	HTTPClient *http.Client
	Timeout    time.Duration
}

type SpeechRequest struct {
	Text         string
	Model        string
	Voice        string
	Format       string
	Instructions string
	Speed        *float64
}

type SpeechResult struct {
	Audio      []byte
	Model      string
	Voice      string
	Format     string
	TextSHA256 string
}

var speechRetrySleep = sleepSpeechRetry

type SpeechClient struct {
	mu      sync.Mutex
	active  sync.WaitGroup
	baseURL url.URL
	apiKey  []byte
	http    *http.Client
	closed  bool
}

func NewSpeechClient(options SpeechClientOptions) (*SpeechClient, error) {
	baseURL := options.BaseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultSpeechBaseURL
	}
	parsed, err := normalizeSpeechBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if len(options.APIKey) == 0 {
		return nil, errors.New("speech API key is required")
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultSpeechTimeout
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	} else {
		clone := *httpClient
		if clone.Timeout == 0 {
			clone.Timeout = timeout
		}
		httpClient = &clone
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &SpeechClient{baseURL: *parsed, apiKey: bytes.Clone(options.APIKey), http: httpClient}, nil
}

func (client *SpeechClient) Close() {
	client.mu.Lock()
	client.closed = true
	client.mu.Unlock()
	client.active.Wait()
	client.mu.Lock()
	clear(client.apiKey)
	client.apiKey = nil
	client.mu.Unlock()
}

func (client *SpeechClient) Synthesize(ctx context.Context, request SpeechRequest) (SpeechResult, error) {
	authorization, err := client.begin()
	if err != nil {
		return SpeechResult{}, err
	}
	defer client.active.Done()
	resolved := normalizeSpeechRequest(request)
	if resolved.Text == "" {
		return SpeechResult{}, errors.New("speech input text is required")
	}
	if resolved.Format != "wav" {
		return SpeechResult{}, fmt.Errorf("speech format must be wav, got %q", resolved.Format)
	}
	payload := map[string]any{
		"model":           resolved.Model,
		"input":           resolved.Text,
		"voice":           resolved.Voice,
		"response_format": resolved.Format,
	}
	if strings.TrimSpace(resolved.Instructions) != "" {
		payload["instructions"] = resolved.Instructions
	}
	if resolved.Speed != nil {
		if *resolved.Speed < 0.25 || *resolved.Speed > 4 {
			return SpeechResult{}, errors.New("speech speed must be between 0.25 and 4")
		}
		payload["speed"] = *resolved.Speed
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SpeechResult{}, err
	}
	endpoint := client.baseURL
	endpoint.Path += "/audio/speech"
	for attempt := 1; attempt <= speechMaxAttempts; attempt++ {
		result, retry, retryAfter, err := client.synthesizeAttempt(ctx, endpoint.String(), authorization, body, resolved)
		if err == nil || !retry || attempt == speechMaxAttempts || ctx.Err() != nil {
			return result, err
		}
		wait := retryAfter
		if wait <= 0 {
			wait = speechBackoffDelay(attempt)
		}
		if sleepErr := speechRetrySleep(ctx, wait); sleepErr != nil {
			return SpeechResult{}, sleepErr
		}
	}
	return SpeechResult{}, errors.New("speech synthesis retry loop exhausted")
}

func (client *SpeechClient) synthesizeAttempt(ctx context.Context, endpoint string, authorization string, body []byte, resolved SpeechRequest) (SpeechResult, bool, time.Duration, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SpeechResult{}, false, 0, errors.New("speech request: invalid configuration")
	}
	httpRequest.Header.Set("Accept", "audio/*")
	httpRequest.Header.Set("Authorization", authorization)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(httpRequest)
	httpRequest.Header.Del("Authorization")
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return SpeechResult{}, true, 0, errors.New("speech request failed")
	}
	if response == nil || response.Body == nil {
		return SpeechResult{}, true, 0, errors.New("speech response is incomplete")
	}
	defer response.Body.Close()
	audio, readErr := io.ReadAll(io.LimitReader(response.Body, maxSpeechBytes+1))
	if readErr != nil {
		clear(audio)
		return SpeechResult{}, true, 0, errors.New("read speech response failed")
	}
	if len(audio) > maxSpeechBytes {
		clear(audio)
		return SpeechResult{}, false, 0, errors.New("speech response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retry := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		clear(audio)
		return SpeechResult{}, retry, retryAfter, fmt.Errorf("speech request returned HTTP %d", response.StatusCode)
	}
	if len(audio) == 0 {
		return SpeechResult{}, false, 0, errors.New("speech response is empty")
	}
	return SpeechResult{
		Audio: audio, Model: resolved.Model, Voice: resolved.Voice,
		Format: resolved.Format, TextSHA256: sha256String(resolved.Text),
	}, false, 0, nil
}

func (client *SpeechClient) begin() (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return "", errors.New("speech client is closed")
	}
	client.active.Add(1)
	return "Bearer " + string(client.apiKey), nil
}

func normalizeSpeechBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("speech base URL must be an absolute HTTP(S) URL without credentials, escaped path, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/v1"
	}
	return parsed, nil
}

func normalizeSpeechRequest(request SpeechRequest) SpeechRequest {
	request.Text = strings.TrimSpace(request.Text)
	if strings.TrimSpace(request.Model) == "" {
		request.Model = DefaultSpeechModel
	}
	if strings.TrimSpace(request.Voice) == "" {
		request.Voice = DefaultSpeechVoice
	}
	if strings.TrimSpace(request.Format) == "" {
		request.Format = DefaultSpeechFormat
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Voice = strings.TrimSpace(request.Voice)
	request.Format = strings.ToLower(strings.TrimSpace(request.Format))
	return request
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if !retryAt.After(now) {
		return 0
	}
	return retryAt.Sub(now)
}

func speechBackoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := speechRetryBaseDelay << (attempt - 1)
	if delay > speechRetryMaxDelay {
		delay = speechRetryMaxDelay
	}
	jitterBound := delay / 2
	if jitterBound <= 0 {
		return delay
	}
	jitter := time.Duration(time.Now().UnixNano() % int64(jitterBound))
	if delay+jitter > speechRetryMaxDelay {
		return speechRetryMaxDelay
	}
	return delay + jitter
}

func sleepSpeechRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
