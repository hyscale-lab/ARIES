package openclaw

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/internal/testfixture"
	"github.com/hyscale-lab/aries/pkg/core"
)

// The local fixture substitutes only the fixed alias destination so unit tests
// exercise the real startup script without writing outside their temp directory.
type workspaceExecutor struct {
	recordingExecutor
	workdir, alias string
	calls          int
}

func (s *workspaceExecutor) Workdir() string { return s.workdir }
func (s *workspaceExecutor) ExecStream(ctx context.Context, c core.Command, input io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	s.calls++
	if c.Path != remoteShell || c.User != "0:0" || c.Dir != "/" || !matchesExactArgv(c.Args, "-c", prepareWorkspaceScript, "aries-openclaw-workspace", s.workdir, virtualWorkspace) {
		return core.CommandResult{}, errors.New("unexpected workspace setup command")
	}
	command := exec.CommandContext(ctx, c.Path, "-c", prepareWorkspaceScript, "aries-openclaw-workspace", s.workdir, s.alias)
	command.Stdin, command.Stdout, command.Stderr = input, stdout, stderr
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return core.CommandResult{ExitCode: exit.ExitCode()}, nil
	}
	return core.CommandResult{}, err
}

func TestWorkspaceStartupAndRevocation(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "task with ' quotes")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(base, "task-link")
	if err := os.Symlink(workdir, logical); err != nil {
		t.Fatal(err)
	}
	executor := &workspaceExecutor{workdir: logical, alias: filepath.Join(base, "aries", "runtime", "workspace")}
	dialect := &Dialect{}
	fixture := testfixture.New(t, bridgessh.Options{Dialect: dialect, OutputDir: t.TempDir()})
	server := &Server{manager: fixture.Manager, dialect: dialect}
	if _, err := server.StartTarget(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	if dialect.validationRoot != workdir {
		t.Fatalf("canonical root = %q", dialect.validationRoot)
	}
	validation, refusal := dialect.Prepare(encodeCanonicalTokens([]string{remoteShell, "-c", workdirValidationScript, workdirValidationLabel, virtualWorkspace, virtualWorkspace}), logical)
	if refusal != nil || validation.Command.Dir != logical || validation.Command.Args[3] != workdir || validation.Command.Args[4] != workdir {
		t.Fatalf("physical validation must preserve the sandbox process cwd: %#v, %#v", validation, refusal)
	}
	if _, err := server.StartTarget(context.Background(), executor); err == nil {
		t.Fatal("second start accepted")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.StartTarget(context.Background(), executor); err == nil {
		t.Fatal("start after stop accepted")
	}
	if target, err := os.Readlink(executor.alias); err != nil || target != workdir || executor.calls != 1 {
		t.Fatalf("alias after Stop = %q, %v; setup calls %d", target, err, executor.calls)
	}
	if err := os.WriteFile(filepath.Join(executor.alias, "after-stop"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(workdir, "after-stop")); err != nil || string(data) != "candidate" {
		t.Fatalf("alias no longer reaches candidate: %q %v", data, err)
	}
}

func TestWorkspaceStartupRefusesCollisionsBeforeListening(t *testing.T) {
	for _, collision := range []string{"file", "directory", "symlink", "dangling-symlink", "ancestor-symlink", "ancestor-file"} {
		t.Run(collision, func(t *testing.T) {
			base := t.TempDir()
			workdir := filepath.Join(base, "task")
			if err := os.Mkdir(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(base, "runtime")
			alias := filepath.Join(parent, "workspace")
			var err error
			switch collision {
			case "ancestor-symlink":
				err = os.Symlink(workdir, parent)
			case "ancestor-file":
				err = os.WriteFile(parent, []byte("original"), 0o600)
			default:
				if err = os.Mkdir(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				switch collision {
				case "file":
					err = os.WriteFile(alias, []byte("original"), 0o600)
				case "directory":
					err = os.Mkdir(alias, 0o700)
				case "symlink":
					err = os.Symlink(workdir, alias)
				case "dangling-symlink":
					err = os.Symlink(filepath.Join(base, "missing"), alias)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			watched := alias
			if strings.HasPrefix(collision, "ancestor-") {
				watched = parent
			}
			before, err := os.Lstat(watched)
			if err != nil {
				t.Fatal(err)
			}
			listened := false
			dialect := &Dialect{}
			fixture := testfixture.New(t, bridgessh.Options{Dialect: dialect, OutputDir: t.TempDir(), ResolveListen: func(context.Context) (core.BridgeListen, error) {
				listened = true
				return core.BridgeListen{BindHost: "127.0.0.1"}, nil
			}})
			server := &Server{manager: fixture.Manager, dialect: dialect}
			_, err = server.StartTarget(context.Background(), &workspaceExecutor{workdir: workdir, alias: alias})
			if err == nil || listened {
				t.Fatalf("collision start = %v; listened %v", err, listened)
			}
			after, err := os.Lstat(watched)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("existing entry changed: %v", err)
			}
			if before.Mode().IsRegular() {
				data, err := os.ReadFile(watched)
				if err != nil || string(data) != "original" {
					t.Fatalf("existing content changed: %q %v", data, err)
				}
			}
		})
	}
}

func TestMappedValidationRunsNativeDirectoryChecks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub dir"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		suffix string
		valid  bool
	}{{"", true}, {"/sub dir", true}, {"/missing", false}, {"/link", false}, {"/../escape", false}} {
		plan := prepareRemoteCommand(remoteCommand{argv: []string{remoteShell, "-c", workdirValidationScript, workdirValidationLabel, virtualWorkspace + tc.suffix, virtualWorkspace}}, root)
		command := exec.Command(plan.command.Path, plan.command.Args...)
		output, err := command.Output()
		if (err == nil) != tc.valid {
			t.Fatalf("validation %q: %s %v", tc.suffix, output, err)
		}
		if tc.valid && string(bytes.TrimSuffix(output, []byte{'\n'})) != root+tc.suffix {
			t.Fatalf("validated directory = %q", output)
		}
	}
}
