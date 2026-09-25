//go:build integration

package hermesgrpc

// The end-to-end counterpart to pkg/bridge/hermesssh/integration_test.go's
// TestBridgeExecMutatesTheEvaluatorSandbox. It drives the staged client against
// a real Docker sandbox, so the whole path is exercised: argv to payload, mTLS
// with a pinned server, the grammar gate, the sandbox, and the audit.
//
// The pinned Hermes image driving this path through the ARIES plugin is
// covered by pkg/harness/hermes's integration test.

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

// bridgeFixtureImage is the same pinned Debian base the SSH bridge's
// integration test uses: the bridge resolves the bare `bash` token to
// /bin/bash, so a busybox fixture could not run an agent command at all.
const bridgeFixtureImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
}

// buildClient produces the binary the bridge stages, so the test uses the same
// artifact `make build` ships rather than a stand-in.
func buildClient(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aries-grpc")
	build := exec.Command("go", "build", "-o", path, "github.com/hyscale-lab/aries/cmd/aries-grpc")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aries-grpc: %v\n%s", err, output)
	}
	return path
}

func TestClientDrivesTheBridgeIntoARealSandbox(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	outputDir := t.TempDir()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	if err := dockersandbox.PullImages(ctx, []string{bridgeFixtureImage}); err != nil {
		t.Fatalf("prepare pinned bridge fixture image: %v", err)
	}
	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{
		RunID: "hermes-grpc-integration", TaskID: "same-state",
		Environment: core.Environment{Image: bridgeFixtureImage, Workdir: "/work", MemoryMB: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*dockersandbox.Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})

	manager, err := New(Options{OutputDir: outputDir, Logger: logger, ClientPath: buildClient(t)})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	// grpc-go honours HTTPS_PROXY by default, and a proxy would carry every
	// script, its stdin and all output off the task network. The guard can
	// only be exercised here: Go never proxies loopback, so a unit test
	// against 127.0.0.1 would pass whether or not the client opts out. These
	// point at a port nothing is listening on, so an honoured proxy fails the
	// call outright.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	call := func(stdin string, args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := ClientMain(args, strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// Hermes's session bootstrap runs under a login shell and must see the
	// sandbox's own environment, not one ARIES invents.
	code, stdout, stderr := call("", "exec", "--login", "--", "echo $HOME")
	if code != 0 || strings.TrimSpace(stdout) == "" || strings.Contains(stdout, "$HOME") {
		t.Fatalf("home probe: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// An agent command must reach the real container, and its effect must be
	// visible to the evaluator afterwards.
	code, stdout, stderr = call("streamed-input",
		"exec", "--", "cat > /work/bridge-state; cat /work/bridge-state; printf tool-stderr >&2; exit 7")
	if code != 7 {
		t.Fatalf("agent command: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "streamed-input" || !strings.Contains(stderr, "tool-stderr") {
		t.Fatalf("agent command output: stdout=%q stderr=%q", stdout, stderr)
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// The sandbox outlives the bridge: evaluation runs after revocation.
	after, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "printf evaluator > /work/after-bridge"}})
	if err != nil || after.ExitCode != 0 {
		t.Fatalf("sandbox did not survive revocation: %v (exit %d)", err, after.ExitCode)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !os.IsNotExist(err) {
		t.Fatalf("client identity survived revocation: %v", err)
	}

	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2: %#v", len(records), records)
	}
	for index, record := range records {
		if record["operation_class"] != kindAgent || record["status"] != "completed" {
			t.Fatalf("record %d = %#v, want %s/completed", index, record, kindAgent)
		}
	}
	// Command output must never enter the audit.
	for _, record := range records {
		for _, forbidden := range []string{"stdout", "stderr"} {
			if _, present := record[forbidden]; present {
				t.Fatalf("record carries %q: %#v", forbidden, record)
			}
		}
	}
}
