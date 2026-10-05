// Package gateway implements the pinned Hermes native HTTP run protocol.
package gateway

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

const maxResponseBytes = 8 << 20

// Client speaks to one task-local Gateway. It never retries run admission.
type Client struct {
	mu     sync.RWMutex
	closed bool
	base   string
	token  string
	http   *http.Client
}

// Run is a native Hermes run status. Terminal states do not prove runtime absence.
type Run struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	SessionID string `json:"session_id,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

// New constructs a client for a private Gateway endpoint. Redirects are refused
// so credentials and non-idempotent requests cannot move to another endpoint.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid Hermes Gateway endpoint")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid Hermes Gateway credential")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Task-local service credentials must never traverse a host proxy.
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{
		Timeout:       30 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Close releases idle connections and the retained credential. Active requests
// remain governed by their contexts; close after confirmed runtime shutdown.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.token = ""
	c.mu.Unlock()
	c.http.CloseIdleConnections()
}

// Ready checks the authenticated model endpoint, not merely TCP reachability.
func (c *Client) Ready(ctx context.Context) error {
	var models struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/models", nil, http.StatusOK, &models); err != nil {
		return err
	}
	if models.Object != "list" || models.Data == nil {
		return errors.New("invalid Hermes Gateway model catalog")
	}
	return nil
}

// Submit admits exactly one request. A transport error may follow server-side
// admission, so callers must clean up the runtime rather than retry submission.
func (c *Client) Submit(ctx context.Context, input, sessionID string) (string, error) {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(sessionID) == "" {
		return "", errors.New("Hermes Gateway input and session ID are required")
	}
	var admitted struct {
		Run
		Replayed bool `json:"replayed"`
	}
	err := c.request(ctx, http.MethodPost, "/v1/runs", struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id"`
	}{input, sessionID}, http.StatusAccepted, &admitted)
	if err != nil {
		return "", err
	}
	if admitted.Status != "started" || admitted.Replayed || (admitted.SessionID != "" && admitted.SessionID != sessionID) {
		return "", errors.New("invalid Hermes Gateway admission state")
	}
	if !validID(admitted.RunID) {
		return "", errors.New("invalid Hermes Gateway admission ID")
	}
	return admitted.RunID, nil
}

// Status returns a status only when the response identifies the requested run.
func (c *Client) Status(ctx context.Context, runID string) (Run, error) {
	var run Run
	if !validID(runID) {
		return run, errors.New("invalid Hermes Gateway run ID")
	}
	if err := c.request(ctx, http.MethodGet, "/v1/runs/"+runID, nil, http.StatusOK, &run); err != nil {
		return Run{}, err
	}
	if run.RunID != runID {
		return Run{}, errors.New("Hermes Gateway run ID mismatch")
	}
	switch run.Status {
	case "queued", "started", "running", "waiting_for_approval", "stopping", "completed", "failed", "cancelled", "interrupted":
	default:
		return Run{}, errors.New("unknown Hermes Gateway run status")
	}
	return run, nil
}

// Wait polls until native completion, failure, cancellation, or interruption.
// Cancellation stops polling; runtime shutdown remains the caller's obligation.
func (c *Client) Wait(ctx context.Context, runID string, pollInterval time.Duration) (Run, error) {
	if pollInterval <= 0 {
		return Run{}, errors.New("Hermes Gateway poll interval must be positive")
	}
	for {
		run, err := c.Status(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		switch run.Status {
		case "completed", "failed", "cancelled", "interrupted":
			return run, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Run{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// Stop requests cooperative cancellation. Success does not confirm absence of
// agent/tool processes; deployment shutdown is still required before evaluation.
func (c *Client) Stop(ctx context.Context, runID string) error {
	if !validID(runID) {
		return errors.New("invalid Hermes Gateway run ID")
	}
	var run Run
	if err := c.request(ctx, http.MethodPost, "/v1/runs/"+runID+"/stop", nil, http.StatusOK, &run); err != nil {
		return err
	}
	if run.RunID != runID {
		return errors.New("Hermes Gateway stop run ID mismatch")
	}
	switch run.Status {
	case "stopping", "completed", "failed", "cancelled", "interrupted":
		return nil
	default:
		return errors.New("invalid Hermes Gateway stop status")
	}
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (c *Client) request(ctx context.Context, method, path string, input any, want int, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("encode Hermes Gateway request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return errors.New("construct Hermes Gateway request")
	}
	c.mu.RLock()
	token, closed := c.token, c.closed
	c.mu.RUnlock()
	if closed {
		return errors.New("Hermes Gateway client closed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Hermes Gateway HTTP request failed")
	}
	defer resp.Body.Close()
	// Never return an error body: authentication failures can echo credentials.
	if resp.StatusCode != want {
		return fmt.Errorf("Hermes Gateway returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("read Hermes Gateway response")
	}
	if len(data) > maxResponseBytes {
		return errors.New("Hermes Gateway response exceeds size limit")
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("invalid Hermes Gateway JSON response")
	}
	return nil
}
