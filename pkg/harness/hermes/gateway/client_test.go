package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "private-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestNativeRunLifecycle(t *testing.T) {
	var polls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing authentication")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		case "POST /v1/runs":
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"input":"do task","session_id":"session-1"}` {
				t.Errorf("request %s", string(body))
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"run_id":"run-1","status":"started"}`)
		case "GET /v1/runs/run-1":
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"run_id":"run-1","status":"running"}`)
			} else {
				fmt.Fprint(w, `{"run_id":"run-1","session_id":"session-1","status":"completed","output":"done"}`)
			}
		case "POST /v1/runs/run-1/stop":
			fmt.Fprint(w, `{"run_id":"run-1","status":"completed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, err := client.Submit(context.Background(), "do task", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	run, err := client.Wait(context.Background(), id, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || run.Output != "done" || run.SessionID != "session-1" {
		t.Fatalf("run %#v", run)
	}
	if err := client.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalStates(t *testing.T) {
	for _, state := range []string{"failed", "cancelled", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"run_id":"run-1","status":%q,"error":"upstream failure"}`, state)
			})
			run, err := client.Wait(context.Background(), "run-1", time.Millisecond)
			if err != nil || run.Status != state {
				t.Fatalf("%#v %v", run, err)
			}
		})
	}
}

func TestRejectMalformedStatus(t *testing.T) {
	for name, body := range map[string]string{"mismatched ID": `{"run_id":"other","status":"completed"}`, "unknown state": `{"run_id":"run-1","status":"bogus"}`, "malformed JSON": `{"secret":"private-token"`, "oversize": strings.Repeat(" ", maxResponseBytes+1)} {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			_, err := client.Status(context.Background(), "run-1")
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe or missing error %v", err)
			}
		})
	}
}

func TestSubmitNeverRetriesAndRedactsErrors(t *testing.T) {
	for _, code := range []int{401, 403, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var requests atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(code)
				fmt.Fprint(w, "private-token")
			})
			_, err := client.Submit(context.Background(), "task", "session")
			if err == nil || strings.Contains(err.Error(), "private-token") || requests.Load() != 1 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
	_, err := client.Submit(context.Background(), "task", "session")
	if err == nil || forwarded.Load() != 0 {
		t.Fatalf("redirect followed: %v", err)
	}
}

func TestWaitCancellation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"run_id":"run-1","status":"running"}`) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Wait(ctx, "run-1", time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
}

func TestCloseAndInvalidIDs(t *testing.T) {
	var requests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	for _, id := range []string{"", "../secret", "run?a=b", "run/stop"} {
		if _, err := client.Status(context.Background(), id); err == nil {
			t.Errorf("accepted %q", id)
		}
		if err := client.Stop(context.Background(), id); err == nil {
			t.Errorf("accepted stop %q", id)
		}
	}
	client.Close()
	if err := client.Ready(context.Background()); err == nil {
		t.Error("closed client accepted request")
	}
	if requests.Load() != 0 {
		t.Fatal("unexpected HTTP request")
	}
	if client.token != "" {
		t.Fatal("retained credential")
	}
}

func TestQueuedRunAndCooperativeStop(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"run_id":"run-1","status":"stopping"}`)
			return
		}
		fmt.Fprint(w, `{"run_id":"run-1","status":"queued"}`)
	})
	if run, err := client.Status(context.Background(), "run-1"); err != nil || run.Status != "queued" {
		t.Fatalf("%#v %v", run, err)
	}
	if err := client.Stop(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
}

func TestCancelInflightRequest(t *testing.T) {
	entered := make(chan struct{})
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Ready(ctx) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestReadyRejectsUnauthenticatedOrMalformedCatalog(t *testing.T) {
	for _, body := range []string{`{}`, `{"object":"list"}`, `{"object":"other","data":[]}`} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if err := client.Ready(context.Background()); err == nil {
			t.Errorf("accepted catalog %s", body)
		}
	}
}

func TestPrivateTransport(t *testing.T) {
	client, err := New("http://127.0.0.1:8642", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport || transport.Proxy != nil {
		t.Fatal("Gateway must own transport and bypass host proxies")
	}
}

func TestRejectUnexpectedAdmission(t *testing.T) {
	for _, body := range []string{
		`{"run_id":"run-1","status":"completed"}`,
		`{"run_id":"run-1","status":"started","replayed":true}`,
		`{"run_id":"run-1","status":"started","session_id":"other"}`,
		`{"run_id":"../bad","status":"started"}`,
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202); fmt.Fprint(w, body) })
		if _, err := client.Submit(context.Background(), "task", "session"); err == nil {
			t.Fatalf("accepted admission %s", body)
		}
	}
}
