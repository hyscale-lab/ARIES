//go:build integration

package hermes

import (
	"github.com/hyscale-lab/aries/internal/testutil/dockerroute"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"

	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
)

// Exercise the native Gateway configuration loader and actual model requests.
func TestRequestSettingsReachRealHermes(t *testing.T) {
	requireDockerImage(t)
	for index, temperature := range []float64{0, 0.7} {
		reasoning := []string{"off", "high"}[index]
		wireReasoning := []string{"none", "high"}[index]
		t.Run(fmt.Sprint(temperature), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			provider, err := dockerdeployment.New(dockerdeployment.Options{})
			if err != nil {
				t.Fatal(err)
			}
			runEnvironment := dockerroute.Environment(t, "temperature-integration")
			environment := runEnvironment.NewTaskEnvironment()
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := environment.Stop(cleanup); err != nil {
					t.Error(err)
				}
				_ = provider.Close()
			})
			connectivity, err := environment.Start(ctx, deployment.TaskEnvironmentRequest{SandboxRequest: core.SandboxRequest{RunID: "temperature-integration", TaskID: "temperature", Environment: core.Environment{AllowNetwork: true}}, RuntimeName: "temperature-fixture"})
			if err != nil {
				t.Fatal(err)
			}
			endpoint, err := dockerroute.Listen(ctx, connectivity.Placement.AttachmentID)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var bodies []map[string]json.RawMessage
			listener, err := net.Listen("tcp4", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				mu.Lock()
				bodies = append(bodies, body)
				mu.Unlock()
				replyFakeCompletion(w, body["stream"], map[string]any{"role": "assistant", "content": "done"}, "stop")
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

				Deployment:     integrationDeployment(t),
				Image:          integrationImage,
				OutputDir:      t.TempDir(),
				StartTimeout:   90 * time.Second,
				CleanupTimeout: 60 * time.Second,
				APIKeyLookup:   func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
			}, ExtraBody: []byte(`{"user":"aries-temperature-regression"}`)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := manager.Stop(cleanup); err != nil {
					t.Error(err)
				}
				_ = manager.Close()
			})
			request := core.HarnessRequest{Connectivity: connectivity, RunID: "temperature-integration", TaskID: "temperature", Endpoint: core.ToolEndpoint{Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Workdir: "/app"}, Model: core.ModelConfig{Provider: "openai", BaseURL: "http://" + net.JoinHostPort(endpoint.AdvertiseHost, port) + "/v1", Model: "aries-deterministic", APIKeyEnv: "ARIES_TEST_MODEL_KEY", ContextLength: 262144, MaxTokens: 32768, Temperature: &temperature, ReasoningEffort: reasoning}}
			if err := manager.Start(ctx, request); err != nil {
				t.Fatal(err)
			}
			result, err := manager.Run(ctx, "Reply with done without using tools.")
			if err != nil || result.Status != core.StatusSucceeded {
				t.Fatalf("Gateway run: %#v, %v", result, err)
			}
			mu.Lock()
			defer mu.Unlock()
			mainRequests := 0
			systemMessages := 0
			for _, body := range bodies {
				var messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal(body["messages"], &messages); err != nil {
					t.Fatal(err)
				}
				for _, message := range messages {
					if message.Role != "system" {
						continue
					}
					systemMessages++
					for _, marker := range []string{"Hermes Agent - Development Guide", "Contribution Rubric", "Per-conversation prompt caching is sacred"} {
						if strings.Contains(string(message.Content), marker) {
							t.Errorf("system prompt unexpectedly contains repository guidance marker %q", marker)
						}
					}
				}
				if len(body["user"]) == 0 {
					continue
				}
				mainRequests++
				var actual *float64
				if err := json.Unmarshal(body["temperature"], &actual); err != nil || actual == nil || *actual != temperature {
					t.Errorf("temperature=%s, want %v", body["temperature"], temperature)
				}
				if string(body["user"]) != `"aries-temperature-regression"` {
					t.Errorf("extra body user=%s", body["user"])
				}
				if string(body["reasoning_effort"]) != fmt.Sprintf("%q", wireReasoning) {
					t.Errorf("reasoning_effort=%s, want %s", body["reasoning_effort"], wireReasoning)
				}
				if string(body["max_tokens"]) != "32768" {
					t.Errorf("max_tokens=%s", body["max_tokens"])
				}
			}
			if systemMessages == 0 {
				t.Fatal("no captured system prompt")
			}
			if mainRequests == 0 {
				t.Fatal("no request preserved configured generation settings")
			}
		})
	}
}

func replyFakeCompletion(w http.ResponseWriter, stream json.RawMessage, message map[string]any, finish string) {
	if string(stream) == "true" {
		if calls, ok := message["tool_calls"].([]any); ok {
			calls[0].(map[string]any)["index"] = 0
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, choice := range []map[string]any{{"index": 0, "delta": message, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
			data, _ := json.Marshal(map[string]any{"id": "aries-test", "object": "chat.completion.chunk", "created": 1, "model": "aries-deterministic", "choices": []any{choice}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "aries-test", "object": "chat.completion", "created": 1, "model": "aries-deterministic", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
}
