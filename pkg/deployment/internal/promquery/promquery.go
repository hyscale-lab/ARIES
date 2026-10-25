// Package promquery runs Prometheus instant queries for the deployment
// providers' resource sources. Both read the same standalone cAdvisor series
// through it, so container measurements mean the same on every backend.
package promquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxResponse = 8 << 20

// Client queries one Prometheus server.
type Client struct {
	http *http.Client
	base *url.URL
}

// New validates the server's base address without contacting it.
func New(address string) (*Client, error) {
	base, err := url.Parse(address)
	if err != nil || base.Host == "" || base.Scheme != "http" && base.Scheme != "https" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("Prometheus address %q must be an http or https URL", address)
	}
	return &Client{http: &http.Client{}, base: base}, nil
}

func (client *Client) Close() {
	client.http.CloseIdleConnections()
}

// Vector runs one instant query and returns its samples keyed by the value of
// label. Samples without that label, or with a non-finite value, are left out.
func (client *Client) Vector(ctx context.Context, expression, label string) (map[string]float64, error) {
	endpoint := client.base.JoinPath("api", "v1", "query")
	endpoint.RawQuery = url.Values{"query": {expression}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Prometheus: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("read Prometheus response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s: %s", response.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var decoded struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if decoded.Status != "success" || decoded.Data.ResultType != "vector" {
		return nil, errors.New("Prometheus query failed: " + decoded.Error)
	}
	values := make(map[string]float64, len(decoded.Data.Result))
	for _, sample := range decoded.Data.Result {
		key := sample.Metric[label]
		text, ok := sample.Value[1].(string)
		if key == "" || !ok {
			continue
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		values[key] = value
	}
	return values, nil
}
