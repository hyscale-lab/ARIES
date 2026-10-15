package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/codex/client"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func TestDialectAdmitsExactlyOneExecutorSession(t *testing.T) {
	dialect := NewDialect("sandbox-1")
	if _, refusal := dialect.Prepare("/bin/sh -c id", "/app"); refusal == nil || refusal.Status != "rejected" {
		t.Fatalf("shell command accepted: %#v", refusal)
	}
	prepared, refusal := dialect.Prepare(client.RemoteCommand, "/app")
	if refusal != nil {
		t.Fatal(refusal.Message)
	}
	stage := StageDir("sandbox-1")
	if prepared.Action != ssh.Execute || prepared.Command.Path != stage+"/codex" || !slices.Equal(prepared.Command.Args, []string{"exec-server", "--listen", "stdio"}) || prepared.Command.Dir != "/app" || prepared.Command.Env["CODEX_HOME"] != stage+"/home" || prepared.Command.User != "" || prepared.OperationClass != OperationClass {
		t.Fatalf("prepared = %#v", prepared)
	}
	if _, refusal := dialect.Prepare(client.RemoteCommand, "/app"); refusal == nil || !strings.Contains(refusal.Message, "already claimed") {
		t.Fatalf("second executor session admitted: %#v", refusal)
	}
}

func TestStageDirIsRootLevelAndSandboxSpecific(t *testing.T) {
	first, second := StageDir("sandbox-1"), StageDir("sandbox-2")
	if first == second || filepath.Dir(first) != "/" || !strings.HasPrefix(first, stagePrefix) || first != StageDir("sandbox-1") {
		t.Fatalf("stage dirs %q %q", first, second)
	}
}

type stagingSandbox struct {
	runner.Sandbox
	commands []core.Command
	uploads  []string
	exitCode int
	err      error
}

func (s *stagingSandbox) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	return core.CommandResult{ExitCode: s.exitCode}, s.err
}
func (s *stagingSandbox) Upload(_ context.Context, source, destination string) error {
	s.uploads = append(s.uploads, source+"->"+destination)
	return nil
}

func TestStagingUploadsAsRootAndConfirmsRemoval(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	staging, err := NewStaging(binary)
	if err != nil {
		t.Fatal(err)
	}
	target := core.BridgeTarget{SandboxID: "sandbox-1"}
	stage := StageDir(target.SandboxID)
	sandbox := &stagingSandbox{}
	if err := staging.Stage(context.Background(), sandbox, target); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sandbox.uploads, []string{binary + "->" + stage + "/codex"}) || len(sandbox.commands) != 2 {
		t.Fatalf("uploads=%v commands=%#v", sandbox.uploads, sandbox.commands)
	}
	// mkdir without -p fails on an existing path instead of reusing it.
	if sandbox.commands[0].Path != "/bin/mkdir" || slices.Contains(sandbox.commands[0].Args, "-p") || sandbox.commands[0].Args[len(sandbox.commands[0].Args)-1] != stage {
		t.Fatalf("stage directory command = %#v", sandbox.commands[0])
	}
	if err := staging.Remove(context.Background(), sandbox, target); err != nil {
		t.Fatal(err)
	}
	removal := sandbox.commands[2]
	if !strings.Contains(removal.Args[1], `test ! -e "$1"`) || removal.Args[len(removal.Args)-1] != stage {
		t.Fatalf("removal does not confirm absence: %#v", removal)
	}
	for _, command := range sandbox.commands {
		if command.User != "0:0" {
			t.Fatalf("staging command not run as root: %#v", command)
		}
	}
	sandbox.exitCode = 1
	if err := staging.Remove(context.Background(), sandbox, target); err == nil {
		t.Fatal("unconfirmed removal accepted")
	}
	sandbox.exitCode, sandbox.err = 0, errors.New("exec failed")
	if err := staging.Remove(context.Background(), sandbox, target); err == nil {
		t.Fatal("failed removal accepted")
	}
}

func TestStagingRejectsNonExecutableSource(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{plain, link, dir, filepath.Join(dir, "missing")} {
		if _, err := NewStaging(source); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
}
