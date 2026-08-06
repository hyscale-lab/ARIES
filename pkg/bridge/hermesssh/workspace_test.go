package hermesssh

import (
	"strings"
	"testing"
)

func TestPrepareMapsAgentCommandsToTheSandboxWorkdir(t *testing.T) {
	command, err := decodeRemoteCommand("bash -c " + shlexQuote("echo hi"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareRemoteCommand(command, "/testbed")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.command.Path != remoteShellPath || prepared.command.Dir != "/testbed" {
		t.Fatalf("prepared = %#v", prepared.command)
	}
	if len(prepared.command.Args) != 2 || prepared.command.Args[0] != "-c" || prepared.command.Args[1] != "echo hi" {
		t.Fatalf("args = %#v", prepared.command.Args)
	}
	if prepared.kind != kindAgent {
		t.Fatalf("kind = %q", prepared.kind)
	}
	// Hermes sends no environment assignments of its own; anything present
	// would mean the grammar let something through.
	if prepared.command.Env != nil {
		t.Fatalf("env = %#v", prepared.command.Env)
	}
}

func TestPreparePreservesLoginShell(t *testing.T) {
	command, err := decodeRemoteCommand("bash -l -c " + shlexQuote("export -p"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareRemoteCommand(command, "/testbed")
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.command.Args) != 3 || prepared.command.Args[0] != "-l" || prepared.command.Args[1] != "-c" {
		t.Fatalf("login args = %#v", prepared.command.Args)
	}
}

func TestPrepareRejectsUnsafeWorkdir(t *testing.T) {
	command, err := decodeRemoteCommand("bash -c " + shlexQuote("echo hi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, workdir := range []string{"", "relative/path", "/has space", "/quote'd", "/trailing/", "/a//b", "/a/../b", "/a/./b"} {
		if _, err := prepareRemoteCommand(command, workdir); err == nil {
			t.Fatalf("workdir %q was accepted", workdir)
		}
	}
	if _, err := prepareRemoteCommand(command, "/"); err != nil {
		t.Fatalf("root workdir rejected: %v", err)
	}
}

// The recorded command must reproduce the exact bytes Hermes put on the wire,
// so the command hash is taken over a canonical, replayable form.
func TestEncodeRoundTripsToTheOriginalPayload(t *testing.T) {
	for _, payload := range []string{
		capturedAgentPayload,
		capturedLoginPayload,
		"bash -c " + shlexQuote("echo hi"),
	} {
		command, err := decodeRemoteCommand(payload)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := prepareRemoteCommand(command, "/testbed")
		if err != nil {
			t.Fatal(err)
		}
		if prepared.encoded != payload {
			t.Fatalf("encoded = %q, want %q", prepared.encoded, payload)
		}
	}
}

func TestPrepareBootstrapProbesReplayLiterally(t *testing.T) {
	for _, payload := range []string{connectionProbePayload, remoteHomePayload} {
		command, err := decodeRemoteCommand(payload)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := prepareRemoteCommand(command, "/testbed")
		if err != nil {
			t.Fatal(err)
		}
		if prepared.command.Path != bootstrapShell || prepared.command.Args[1] != payload {
			t.Fatalf("bootstrap prepared = %#v", prepared.command)
		}
		if prepared.kind != kindBootstrap || prepared.encoded != payload {
			t.Fatalf("bootstrap record = %q / %q", prepared.kind, prepared.encoded)
		}
	}
}

func TestPrepareRejectsUnknownKind(t *testing.T) {
	if _, err := prepareRemoteCommand(remoteCommand{argv: []string{"bash"}, kind: "other"}, "/testbed"); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("err = %v", err)
	}
}
