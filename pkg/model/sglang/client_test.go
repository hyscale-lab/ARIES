package sglang

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBaseURLIsExactAndCanonical(t *testing.T) {
	for input, want := range map[string]string{"http://host/v1": "http://host/v1", "https://host:30000/v1/": "https://host:30000/v1"} {
		normalized, err := NormalizeBaseURL(input)
		if err != nil || normalized != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v", input, normalized, err)
		}
	}
	for _, input := range []string{
		"http://host/v1/v1",
		"http://host/v1?",
		"http:host/v1",
		"http://user@host/v1",
		"http://host/v%31",
		"http://host/v1?query",
		"http://host/v1#",
		"http://host/v1#fragment",
	} {
		if _, err := NormalizeBaseURL(input); err == nil {
			t.Fatalf("accepted ambiguous base URL %q", input)
		}
	}
}

func TestModelsUsesVersionedRouteAndOptionalBearer(t *testing.T) {
	for _, key := range [][]byte{nil, []byte("secret")} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/models" {
				t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
			}
			want := ""
			if len(key) != 0 {
				want = "Bearer secret"
			}
			if got := r.Header.Get("Authorization"); got != want {
				t.Fatalf("authorization = %q", got)
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
		}))
		client, err := New(server.URL+"/v1/", key, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		models, err := client.Models(context.Background())
		if err != nil || strings.Join(models, ",") != "model-a,model-b" {
			t.Fatalf("models=%v error=%v", models, err)
		}
		server.Close()
	}
}

type failingReader struct{ canary string }

func (reader failingReader) Read([]byte) (int, error) { return 0, errors.New(reader.canary) }
func (failingReader) Close() error                    { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestClientRejectsAndSanitizesFailures(t *testing.T) {
	const key = "key-canary"
	const body = "body-canary"
	tests := []struct {
		name      string
		transport http.RoundTripper
		operation string
	}{
		{"transport", roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New(body) }), "transport error"},
		{"read", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: failingReader{body}, Request: request}, nil
		}), "response read error"},
		{"status", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		}), "status 503"},
		{"oversized", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBytes+1))), Request: request}, nil
		}), "too large"},
		{"malformed", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		}), "malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := New("http://example.invalid/v1", []byte(key), &http.Client{Transport: test.transport})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Models(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.operation) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), body) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestClientRejectsRedirectAndBadBase(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/v1/models", http.StatusFound) }))
	defer redirect.Close()
	client, _ := New(redirect.URL+"/v1", nil, redirect.Client())
	if _, err := client.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("redirect error = %v", err)
	}
	for _, base := range []string{"", "ftp://host/v1", "http://host", "http://user@host/v1"} {
		if _, err := New(base, nil, nil); err == nil {
			t.Fatalf("accepted base %q", base)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Models(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestCloseClearsOwnedCredentialAndFailsClosed(t *testing.T) {
	input := []byte("owned-secret")
	var requests atomic.Int32
	client, err := New("http://example.invalid/v1", input, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected request")
	})})
	if err != nil {
		t.Fatal(err)
	}
	owned := client.apiKey
	client.Close()
	client.Close()
	if client.apiKey != nil || string(owned) != strings.Repeat("\x00", len(owned)) || string(input) != "owned-secret" {
		t.Fatalf("credential cleanup: owned=%q input=%q current=%v", owned, input, client.apiKey)
	}
	if _, err := client.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Models after Close error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests after Close = %d", requests.Load())
	}
}

func TestCloseWaitsForConcurrentRequestAndThenFailsClosed(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", []byte("secret"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := client.Models(context.Background())
		requestDone <- err
	}()
	<-started
	closeDone := make(chan struct{})
	closeStarted := make(chan struct{})
	go func() {
		close(closeStarted)
		client.Close()
		close(closeDone)
	}()
	<-closeStarted
	select {
	case <-closeDone:
		t.Fatal("Close returned while credential-bearing request was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	<-closeDone
	if _, err := client.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("request after concurrent Close error = %v", err)
	}
}
