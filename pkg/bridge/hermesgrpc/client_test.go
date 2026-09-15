package hermesgrpc

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// TestRemotePayloadRecoversTheCommand pins the one piece of OpenSSH grammar
// the client has to understand. The cases are reasoned from ssh(1)'s option
// set, not from a recording: until the argv Hermes actually emits is captured
// against the pinned image, this is the unverified part of the client.
func TestRemotePayloadRecoversTheCommand(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "separate value options",
			args: []string{"-p", "39425", "-i", "/run/aries/grpc/client.pem", "aries@172.17.0.1", "bash", "-c", "'echo hi'"},
			want: "bash -c 'echo hi'",
		},
		{
			name: "inline value option",
			args: []string{"-p39425", "aries@172.17.0.1", "echo", "$HOME"},
			want: "echo $HOME",
		},
		{
			name: "repeated -o and boolean flags",
			args: []string{"-T", "-o", "StrictHostKeyChecking=accept-new", "-o", "BatchMode=yes", "aries@host", "bash", "-c", "x"},
			want: "bash -c x",
		},
		{
			name: "clustered flags ending in a value option",
			args: []string{"-tp", "39425", "aries@host", "true"},
			want: "true",
		},
		{
			name: "double dash ends the options",
			args: []string{"--", "aries@host", "true"},
			want: "true",
		},
		{
			name: "destination with no command",
			args: []string{"-p", "39425", "aries@172.17.0.1"},
			want: "",
		},
		{
			name: "no operands at all",
			args: []string{"-O", "check"},
			want: "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := remotePayload(testCase.args); got != testCase.want {
				t.Fatalf("remotePayload(%q) = %q, want %q", testCase.args, got, testCase.want)
			}
		})
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
	code := ClientMain(
		[]string{"-p", "1", "aries@host", "bash", "-c", "'echo hi'"},
		strings.NewReader("piped"), &stdout, &stderr)

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

// TestClientMainReportsRefusalAsTransportFailure pins that a denied file sync
// reaches Hermes the way a refused SSH channel request does: exit 255, not a
// command exit code it might mistake for a result.
func TestClientMainReportsRefusalAsTransportFailure(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)

	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	var stdout, stderr bytes.Buffer
	code := ClientMain(
		[]string{"aries@host", "mkdir", "-p", "/root/.hermes"},
		strings.NewReader(""), &stdout, &stderr)

	if code != transportFailureExit {
		t.Fatalf("exit code = %d, want %d", code, transportFailureExit)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a denied sync reached the sandbox")
	}
}

// TestClientMainWithoutACommandDoesNothing covers the connection-setup
// invocation: there is nothing to run, so nothing must be sent.
func TestClientMainWithoutACommandDoesNothing(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)

	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(identityEnv, endpoint.IdentitySourceFile)
	t.Setenv(trustedEnv, endpoint.KnownHostsSourceFile)

	var stdout, stderr bytes.Buffer
	if code := ClientMain([]string{"-N", "aries@host"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a setup invocation reached the sandbox")
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
	code := ClientMain([]string{"aries@host", "bash", "-c", "x"}, strings.NewReader(""), &stdout, &stderr)
	if code != transportFailureExit {
		t.Fatalf("exit code = %d, want %d", code, transportFailureExit)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a call reached the sandbox over an unpinned connection")
	}
}
