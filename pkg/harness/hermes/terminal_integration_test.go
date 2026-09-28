//go:build integration

package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// A terminal call that names no workdir must run in the endpoint's workdir.
// Hermes wraps every command in `builtin cd -- <cwd> || exit 126`; before the
// fix <cwd> was /run/aries/workspace, a path only the harness container has,
// so every such command failed in the sandbox before it ran. The real
// one-shot runs against a fake model that asks for one terminal call, and an
// `ssh` stand-in on PATH records the command Hermes sends instead of reaching
// a bridge; the test reads the directory that command cds into.
func TestTerminalCallWithoutWorkdirRunsInTheEndpointWorkdir(t *testing.T) {
	const workdir = "/workspace/aries-terminal-regression"
	for _, image := range []string{integrationImage, "docker.io/nousresearch/hermes-agent:v2026.8.31"} {
		t.Run(filepath.Base(image), func(t *testing.T) {
			if image == integrationImage {
				requireDockerImage(t)
			}
			manager, err := New(Options{
				Image: image, OutputDir: t.TempDir(),
				StartTimeout: 90 * time.Second, CleanupTimeout: 60 * time.Second,
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
			if err := os.WriteFile(identity, []byte("integration identity"), 0o600); err != nil {
				t.Fatal(err)
			}
			request := core.HarnessRequest{
				RunID: "terminal-integration", TaskID: "terminal",
				Endpoint: core.ToolEndpoint{Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Network: "bridge", IdentitySourceFile: identity, Workdir: workdir},
				Model:    core.ModelConfig{Provider: "openai", BaseURL: "http://127.0.0.1:18080/v1", Model: "aries-deterministic", APIKeyEnv: "ARIES_TEST_MODEL_KEY"},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := manager.Start(ctx, request); err != nil {
				t.Fatal(err)
			}
			result, err := manager.execAttached(ctx, manager.active.containerID, []string{"python3", "-c", terminalWorkdirDriver}, workspaceRoot)
			if err != nil || result.exitCode != 0 {
				t.Fatalf("real one-shot: %v, exit=%d\n%s\n%s", err, result.exitCode, result.stdout, result.stderr)
			}
			var outcome struct {
				Sent    []string `json:"sent"`
				Results []string `json:"results"`
				Exit    int      `json:"exit"`
				Stderr  string   `json:"stderr"`
			}
			if err := json.Unmarshal(result.stdout, &outcome); err != nil {
				t.Fatalf("decode recorded commands: %v\n%s", err, result.stdout)
			}
			wrapped := 0
			for _, command := range outcome.Sent {
				if !strings.Contains(command, "|| exit 126") {
					continue // the session probes and snapshot
				}
				wrapped++
				if !strings.Contains(command, "builtin cd -- "+workdir+" || exit 126") {
					t.Errorf("terminal command does not cd into %s:\n%s", workdir, command)
				}
			}
			if wrapped == 0 {
				t.Fatalf("the terminal call never reached ssh (one-shot exit %d); commands sent:\n%s\ntool results:\n%s\nstderr:\n%s",
					outcome.Exit, strings.Join(outcome.Sent, "\n"), strings.Join(outcome.Results, "\n"), outcome.Stderr)
			}
		})
	}
}

const terminalWorkdirDriver = `
import http.server, json, os, subprocess, threading
# The one-shot drops to the unprivileged hermes user, which may not open a
# root-owned file in sticky /tmp (fs.protected_regular), so the stand-in
# creates its log in a directory of its own.
log_dir = "/tmp/aries-fake-ssh"
log = log_dir + "/commands"
bin_dir = "/tmp/aries-fake-bin"
for directory, mode in ((log_dir, 0o777), (bin_dir, 0o755)):
    os.makedirs(directory, exist_ok=True)
    os.chmod(directory, mode)
# Stand-in for the bridge: skip the options and the destination, record the
# remote command, and run nothing.
with open(os.path.join(bin_dir, "ssh"), "w") as script:
    script.write("#!/bin/sh\n"
                 "while [ $# -gt 0 ]; do case \"$1\" in *@*) shift; break;; *) shift;; esac; done\n"
                 "[ $# -gt 0 ] && printf '%s\\0' \"$*\" >> " + log + "\n"
                 "case \"$*\" in *\"echo \\$HOME\"*) echo /root;; *\"SSH connection established\"*) echo 'SSH connection established';; esac\n"
                 "exit 0\n")
os.chmod(os.path.join(bin_dir, "ssh"), 0o755)
tool_results = []
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def reply(self, payload, content_type):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        results = [m for m in body.get("messages") or [] if m.get("role") == "tool"]
        tool_results.extend(str(m.get("content"))[:500] for m in results)
        offered = any((tool.get("function") or {}).get("name") == "terminal" for tool in body.get("tools") or [])
        if offered and not results:
            message = {"role": "assistant", "content": None, "tool_calls": [{"id": "call_terminal", "type": "function",
                       "function": {"name": "terminal", "arguments": json.dumps({"command": "pwd"})}}]}
            finish = "tool_calls"
        else:
            message, finish = {"role": "assistant", "content": "done"}, "stop"
        usage = {"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11}
        if body.get("stream"):
            # v2026.5.29.2 streams its requests; a plain reply is discarded.
            delta = dict(message)
            if "tool_calls" in delta:
                delta["tool_calls"] = [dict(call, index=0) for call in delta["tool_calls"]]
            chunks = [{"id": "aries-test", "object": "chat.completion.chunk", "created": 0, "model": "aries-deterministic",
                       "choices": [{"index": 0, "delta": delta, "finish_reason": None}]},
                      {"id": "aries-test", "object": "chat.completion.chunk", "created": 0, "model": "aries-deterministic",
                       "choices": [{"index": 0, "delta": {}, "finish_reason": finish}], "usage": usage}]
            stream = "".join("data: " + json.dumps(chunk) + "\n\n" for chunk in chunks) + "data: [DONE]\n\n"
            self.reply(stream.encode(), "text/event-stream")
            return
        self.reply(json.dumps({"id": "aries-test", "object": "chat.completion", "created": 0, "model": "aries-deterministic",
            "choices": [{"index": 0, "message": message, "finish_reason": finish}], "usage": usage}).encode(), "application/json")
server = http.server.HTTPServer(("127.0.0.1", 18080), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    env = dict(os.environ, PATH=bin_dir + ":" + os.environ.get("PATH", ""))
    result = subprocess.run(["/run/aries/run-agent", "aries-deterministic", "custom", "Run pwd in the terminal."],
                            env=env, capture_output=True, text=True, timeout=120)
    sent = open(log).read().split("\\0") if os.path.exists(log) else []
    print(json.dumps({"sent": [c for c in sent if c], "results": tool_results, "exit": result.returncode, "stderr": result.stderr[-3000:]}))
finally:
    server.shutdown()
    server.server_close()
    thread.join()
`
