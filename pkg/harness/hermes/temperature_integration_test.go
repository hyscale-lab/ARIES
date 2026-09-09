//go:build integration

package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Exercise the real one-shot configuration loader and OpenAI SDK serialization,
// not merely the YAML shape. The fake endpoint stays inside each task container.
func TestTemperatureReachesRealHermesRequest(t *testing.T) {
	for _, temperature := range []float64{0, 0.7} {
		t.Run(fmt.Sprint(temperature), func(t *testing.T) {
			manager, err := New(Options{
				Image: "docker.io/nousresearch/hermes-agent:v2026.8.31", OutputDir: t.TempDir(),
				StartTimeout: 90 * time.Second, CleanupTimeout: 60 * time.Second,
				ExtraBody:    []byte(`{"user":"aries-temperature-regression"}`),
				APIKeyLookup: func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				if err := manager.Stop(ctx); err != nil {
					t.Errorf("stop Hermes: %v", err)
				}
				if err := manager.Close(); err != nil {
					t.Errorf("close Hermes: %v", err)
				}
			})
			identity := filepath.Join(t.TempDir(), "id_ed25519")
			if err := os.WriteFile(identity, []byte("integration identity"), 0600); err != nil {
				t.Fatal(err)
			}
			request := core.HarnessRequest{
				RunID: "temperature-integration", TaskID: "temperature",
				Endpoint: core.ToolEndpoint{Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Network: "bridge", IdentitySourceFile: identity},
				Model:    core.ModelConfig{Provider: "openai", BaseURL: "http://127.0.0.1:18080/v1", Model: "aries-deterministic", APIKeyEnv: "ARIES_TEST_MODEL_KEY", Temperature: &temperature},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := manager.Start(ctx, request); err != nil {
				t.Fatal(err)
			}
			result, err := manager.execAttached(ctx, manager.active.containerID, []string{"python3", "-c", temperatureRequestDriver}, workspaceRoot)
			if err != nil || result.exitCode != 0 {
				t.Fatalf("real one-shot: %v, exit=%d\n%s\n%s", err, result.exitCode, result.stdout, result.stderr)
			}
			var bodies []map[string]json.RawMessage
			if err := json.Unmarshal(result.stdout, &bodies); err != nil {
				t.Fatalf("decode requests: %v\n%s", err, result.stdout)
			}
			if len(bodies) == 0 {
				t.Fatal("Hermes sent no completion requests")
			}
			mainRequests := 0
			for _, body := range bodies {
				// Hermes may also issue auxiliary probes without profile overrides.
				if len(body["user"]) == 0 {
					continue
				}
				mainRequests++
				var actual *float64
				if err := json.Unmarshal(body["temperature"], &actual); err != nil || actual == nil || *actual != temperature {
					t.Errorf("request temperature=%s, want %v", body["temperature"], temperature)
				}
				if string(body["user"]) != `"aries-temperature-regression"` {
					t.Errorf("extra_body user=%s", body["user"])
				}
			}
			if mainRequests == 0 {
				t.Fatal("no request preserved the extra_body user field")
			}
		})
	}
}

const temperatureRequestDriver = `
import http.server, json, subprocess, threading
bodies = []
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        assert self.path == "/v1/chat/completions", self.path
        bodies.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
        payload = json.dumps({"id":"aries-test", "object":"chat.completion", "created":0,
            "model":"aries-deterministic", "choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
            "usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
server = http.server.HTTPServer(("127.0.0.1", 18080), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    result = subprocess.run(["/run/aries/run-agent", "aries-deterministic", "custom", "Reply with done without using tools."], capture_output=True, text=True, timeout=90)
    assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
    print(json.dumps(bodies))
finally:
    server.shutdown()
    server.server_close()
    thread.join()
`
