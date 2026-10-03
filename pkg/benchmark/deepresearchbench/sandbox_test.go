package deepresearchbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

type prepareSandboxFake struct {
	commands []core.Command
	results  []core.CommandResult
	errors   []error
	uploads  int
}

func (s *prepareSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	index := len(s.commands)
	s.commands = append(s.commands, command)
	var result core.CommandResult
	if index < len(s.results) {
		result = s.results[index]
	}
	var err error
	if index < len(s.errors) {
		err = s.errors[index]
	}
	return result, err
}

func (s *prepareSandboxFake) Upload(context.Context, string, string) error {
	s.uploads++
	return nil
}

func (*prepareSandboxFake) Download(context.Context, string, string) error { return nil }

func TestPrepareSandboxRemovesThenProvesReportPathAbsent(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	// Command 3 starts SearXNG, command 4 is the first (immediately
	// successful) readiness probe.
	sandbox := &prepareSandboxFake{results: []core.CommandResult{{}, {}, {}, {}}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.commands) != 4 {
		t.Fatalf("commands = %#v", sandbox.commands)
	}
	remove := sandbox.commands[0]
	if remove.Path != "/bin/rm" || !reflect.DeepEqual(remove.Args, []string{"-rf", "--", reportPath}) {
		t.Fatalf("remove command = %#v", remove)
	}
	probe := sandbox.commands[1]
	if probe.Path != "/bin/sh" || len(probe.Args) != 4 || probe.Args[0] != "-c" || !strings.Contains(probe.Args[1], `! -e "$path"`) || !strings.Contains(probe.Args[1], `! -L "$path"`) || probe.Args[2] != "aries-report-absence" || probe.Args[3] != reportPath {
		t.Fatalf("absence probe = %#v", probe)
	}
	start := sandbox.commands[2]
	if start.Path != "/bin/sh" || len(start.Args) != 2 || start.Args[0] != "-c" || start.Args[1] != searxngStartScript {
		t.Fatalf("SearXNG start command = %#v", start)
	}
	health := sandbox.commands[3]
	if health.Path != "/bin/sh" || len(health.Args) != 2 || health.Args[0] != "-c" || !strings.Contains(health.Args[1], searxngHealthCheckURL) {
		t.Fatalf("SearXNG health check command = %#v", health)
	}
	if sandbox.uploads != 0 {
		t.Fatalf("preparation uploaded %d files", sandbox.uploads)
	}
}

func TestPrepareSandboxRetriesSearXNGHealthCheckThenSucceeds(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	restoreDelay := searxngHealthCheckDelay
	searxngHealthCheckDelay = time.Millisecond
	defer func() { searxngHealthCheckDelay = restoreDelay }()

	// rm, absence probe, and start all succeed; the first two health checks
	// fail (SearXNG still starting up), the third succeeds.
	sandbox := &prepareSandboxFake{results: []core.CommandResult{
		{}, {}, {},
		{ExitCode: 7}, {ExitCode: 7}, {},
	}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.commands) != 6 {
		t.Fatalf("commands = %#v, want 3 setup commands + 3 health checks", sandbox.commands)
	}
}

func TestPrepareSandboxFailsClosedWhenSearXNGNeverReady(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	restoreAttempts, restoreDelay := searxngHealthCheckAttempts, searxngHealthCheckDelay
	searxngHealthCheckAttempts = 2
	searxngHealthCheckDelay = time.Millisecond
	defer func() { searxngHealthCheckAttempts, searxngHealthCheckDelay = restoreAttempts, restoreDelay }()

	sandbox := &prepareSandboxFake{results: []core.CommandResult{
		{}, {}, {},
		{ExitCode: 7}, {ExitCode: 7},
	}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err == nil {
		t.Fatal("PrepareSandbox accepted a SearXNG instance that never became ready")
	}
}

func TestPrepareSandboxFailsClosedWhenSearXNGStartFails(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &prepareSandboxFake{results: []core.CommandResult{{}, {}, {ExitCode: 1}}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err == nil {
		t.Fatal("PrepareSandbox accepted a failing SearXNG start command")
	}
}

func TestPrepareSandboxFailsClosedOnRemoveError(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &prepareSandboxFake{errors: []error{errors.New("boom")}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err == nil {
		t.Fatal("PrepareSandbox accepted a failing remove command")
	}
}

func TestPrepareSandboxFailsClosedOnNonZeroExit(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &prepareSandboxFake{results: []core.CommandResult{{}, {ExitCode: 1}}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, sandbox); err == nil {
		t.Fatal("PrepareSandbox accepted a non-zero absence probe")
	}
}

func TestPrepareSandboxRequiresLiveSandbox(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, nil); err == nil {
		t.Fatal("PrepareSandbox accepted a nil sandbox")
	}
}

func (s *prepareSandboxFake) NetworkName() string { return "test-network" }
