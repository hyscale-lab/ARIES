package hermesgrpc

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hyscale-lab/aries/pkg/core"
)

// TestClientRejectsMalformedInvocations pins the argv contract the plugin relies on:
// a known mode, one operand, nothing sent otherwise.
func TestClientRejectsMalformedInvocations(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	for _, args := range [][]string{
		nil,
		{"aries@host", "bash", "-c", "true"},
		{"exec"},
		{"exec", "true", "extra"},
		{"exec", "--unknown", "true"},
	} {
		var stdout, stderr bytes.Buffer
		if code := ClientMain(args, strings.NewReader(""), &stdout, &stderr); code != transportFailureExit {
			t.Fatalf("ClientMain(%q) = %d, want %d", args, code, transportFailureExit)
		}
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a malformed invocation reached the sandbox")
	}
}

// TestClientMainRunsThroughTheBridge drives the staged client against a live
// bridge, so the credential loading, the payload reconstruction, the call and
// the exit code are all exercised as the container will exercise them.
func TestClientMainRunsThroughTheBridge(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 7, Stdout: "out", Stderr: "err"}}
	manager, endpoint := startBridge(t, sandbox)

	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	var stdout, stderr bytes.Buffer
	code := ClientMain([]string{"exec", "--", "echo hi"}, strings.NewReader("piped"), &stdout, &stderr)

	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (stderr: %s)", code, stderr.String())
	}
	if stdout.String() != "out" || !strings.Contains(stderr.String(), "err") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	commands := sandbox.snapshot()
	if len(commands) != 1 || commands[0].Path != remoteShellPath {
		t.Fatalf("commands = %#v", commands)
	}
	if len(commands[0].Args) != 2 || commands[0].Args[1] != "echo hi" {
		t.Fatalf("args = %#v", commands[0].Args)
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if len(sandbox.stdins) != 1 || string(sandbox.stdins[0]) != "piped" {
		t.Fatalf("stdin = %q", sandbox.stdins)
	}
	_ = manager
}

// TestClientRunsLoginScriptsUnderALoginShell pins the flag Hermes's session
// bootstrap needs: the script must reach the sandbox as `bash -l -c`.
func TestClientRunsLoginScriptsUnderALoginShell(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	var stdout, stderr bytes.Buffer
	if code := ClientMain([]string{"exec", "--login", "--", "pwd -P"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr.String())
	}
	commands := sandbox.snapshot()
	if len(commands) != 1 || strings.Join(commands[0].Args, " ") != "-l -c pwd -P" {
		t.Fatalf("commands = %#v", commands)
	}
}

// TestClientMainReportsRevocationAsTransportFailure pins that a call after the
// bridge is revoked reaches Hermes as exit 255, not as a command exit code.
func TestClientMainReportsRevocationAsTransportFailure(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)

	// Revocation deletes the credentials; keep copies so the call fails at the
	// bridge rather than at loading them.
	directory := t.TempDir()
	identity, trusted := filepath.Join(directory, "client.pem"), filepath.Join(directory, "server.crt")
	for source, target := range map[string]string{endpoint.IdentitySourceFile: identity, endpoint.KnownHostsSourceFile: trusted} {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, identity)
	t.Setenv(trustedEnv, trusted)

	var stdout, stderr bytes.Buffer
	if code := ClientMain([]string{"exec", "true"}, strings.NewReader(""), &stdout, &stderr); code != transportFailureExit {
		t.Fatalf("exit code = %d, want %d", code, transportFailureExit)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a call after revocation reached the sandbox")
	}
}

// TestClientRefusesAnUnpinnedServer proves the client authenticates the bridge
// rather than accepting any peer. Hermes's own SSH path accepts the host key
// on first use, so this is stricter than what it replaces.
func TestClientRefusesAnUnpinnedServer(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)

	// A second bridge's certificate is a valid certificate that is not this
	// bridge's, which is exactly the substitution pinning must reject.
	otherSandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	other := newTestManager(t, t.TempDir())
	otherEndpoint, err := other.Start(context.Background(), otherSandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Stop(context.Background()) })

	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, otherEndpoint.KnownHostsSourceFile)

	var stdout, stderr bytes.Buffer
	code := ClientMain([]string{"exec", "x"}, strings.NewReader(""), &stdout, &stderr)
	if code != transportFailureExit {
		t.Fatalf("exit code = %d, want %d", code, transportFailureExit)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a call reached the sandbox over an unpinned connection")
	}
}

// TestClientFileModesRoundTripBytes drives every file mode as the plugin will:
// payload on stdout, one metadata line on stderr, statuses as fixed exit codes.
func TestClientFileModesRoundTripBytes(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{"app/dir": &fstest.MapFile{Mode: fs.ModeDir | 0o755}})
	_, _, endpoint := startFileBridge(t, sandbox, Options{})
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)
	run := func(stdin string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := ClientMain(args, strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	binary := "a\x00b\nline two\n\xff"
	if code, out, meta := run(binary, "file", "write", "/app/f"); code != 0 || out != "" || meta != `{"bytes_written":14,"created":true}`+"\n" {
		t.Fatalf("write: %d %q %q", code, out, meta)
	}
	if code, out, meta := run("", "file", "read", "/app/f"); code != 0 || out != binary || meta != `{"size":14,"truncated":false}`+"\n" {
		t.Fatalf("read: %d %q %q", code, out, meta)
	}
	if code, out, _ := run("", "file", "read", "--offset", "1", "--max-bytes", "2", "/app/f"); code != 0 || out != "\x00b" {
		t.Fatalf("read range: %d %q", code, out)
	}
	if code, out, meta := run("", "file", "lines", "--first", "2", "--max", "1", "--max-line-bytes", "4", "/app/f"); code != 0 || out != "line\n" ||
		meta != `{"ends_with_newline":false,"more":false,"size":14,"total_lines":2}`+"\n" {
		t.Fatalf("lines: %d %q %q", code, out, meta)
	}
	if code, out, meta := run("", "file", "stat", "/app/dir"); code != 0 || out != `{"exists":true,"mode":"0755","size":0,"type":"directory"}`+"\n" || meta != "" {
		t.Fatalf("stat: %d %q %q", code, out, meta)
	}

	for _, test := range []struct {
		args []string
		want int
	}{
		{[]string{"file", "read", "/app/none"}, 1},
		{[]string{"file", "stat", "app/relative"}, 3},
		{[]string{"file", "read", "/app/dir"}, 5},
		{[]string{"file", "copy", "/app/f"}, transportFailureExit},
	} {
		if code, _, message := run("", test.args...); code != test.want || message == "" {
			t.Fatalf("%q: exit %d, want %d (stderr %q)", test.args, code, test.want, message)
		}
	}
}
