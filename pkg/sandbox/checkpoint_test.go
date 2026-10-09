package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

// checkpointDeployment models a runtime that stops when checkpointed and runs
// again when restored.
type checkpointDeployment struct {
	*fakeDeployment
	running       bool
	operations    []string
	checkpointErr error
	stayRunning   bool
	restoreErr    error
	deleteErr     error
}

func (f *checkpointDeployment) Running(context.Context, string) (bool, error) { return f.running, nil }
func (f *checkpointDeployment) Start(ctx context.Context, id string) error {
	f.running = true
	return f.fakeDeployment.Start(ctx, id)
}
func (f *checkpointDeployment) Checkpoint(_ context.Context, _ string, checkpointID string) error {
	f.operations = append(f.operations, "checkpoint "+checkpointID)
	if !f.stayRunning {
		f.running = false
	}
	return f.checkpointErr
}
func (f *checkpointDeployment) Restore(_ context.Context, _ string, checkpointID string) error {
	f.operations = append(f.operations, "restore "+checkpointID)
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.running = true
	return nil
}
func (f *checkpointDeployment) DeleteCheckpoint(_ context.Context, _ string, checkpointID string) error {
	f.operations = append(f.operations, "delete "+checkpointID)
	return f.deleteErr
}

func startCheckpointingSandbox(t *testing.T, f *checkpointDeployment) *Sandbox {
	t.Helper()
	m, err := New(Options{Deployment: f, NewEnvironment: func() deployment.TaskEnvironment { return &fakeEnvironment{f: f.fakeDeployment} }, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Start(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	return s.(*Sandbox)
}

func TestCheckpointRestoreResumesOnlyTheLatestConfirmedCheckpoint(t *testing.T) {
	f := &checkpointDeployment{fakeDeployment: &fakeDeployment{}}
	s := startCheckpointingSandbox(t, f)
	ctx := context.Background()
	for range 2 {
		if err := s.Checkpoint(ctx); err != nil {
			t.Fatalf("Checkpoint() error = %v", err)
		}
		if err := s.Restore(ctx); err != nil {
			t.Fatalf("Restore() error = %v", err)
		}
	}
	if err := s.Restore(ctx); err != nil {
		t.Fatalf("Restore() of a running sandbox error = %v", err)
	}
	want := []string{"checkpoint aries-1", "restore aries-1", "checkpoint aries-2", "delete aries-1", "restore aries-2"}
	if !reflect.DeepEqual(f.operations, want) {
		t.Fatalf("operations = %v, want %v", f.operations, want)
	}
}

func TestFailedCheckpointNeverResumesAnOlderCheckpoint(t *testing.T) {
	f := &checkpointDeployment{fakeDeployment: &fakeDeployment{}}
	s := startCheckpointingSandbox(t, f)
	ctx := context.Background()
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	f.checkpointErr = errors.New("criu dump failed")
	if err := s.Checkpoint(ctx); !errors.Is(err, f.checkpointErr) {
		t.Fatalf("Checkpoint() error = %v, want daemon failure", err)
	}
	if err := s.Restore(ctx); err == nil || !strings.Contains(err.Error(), "without a confirmed checkpoint") {
		t.Fatalf("Restore() error = %v, want refusal to resume aries-1", err)
	}
}

func TestCheckpointRequiresTheContainerToStop(t *testing.T) {
	f := &checkpointDeployment{fakeDeployment: &fakeDeployment{}, stayRunning: true}
	s := startCheckpointingSandbox(t, f)
	if err := s.Checkpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Checkpoint() error = %v, want unconfirmed stop", err)
	}
}

func TestRestoreReportsFailureAndUndeletedCheckpointIsTolerated(t *testing.T) {
	f := &checkpointDeployment{fakeDeployment: &fakeDeployment{}, deleteErr: errors.New("busy")}
	s := startCheckpointingSandbox(t, f)
	ctx := context.Background()
	for range 2 {
		if err := s.Checkpoint(ctx); err != nil {
			t.Fatalf("Checkpoint() error = %v, want deletion failure tolerated", err)
		}
		if err := s.Restore(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	f.restoreErr = errors.New("criu restore failed")
	if err := s.Restore(ctx); !errors.Is(err, f.restoreErr) {
		t.Fatalf("Restore() error = %v, want daemon failure", err)
	}
}

func TestCheckpointRequiresDeploymentCapability(t *testing.T) {
	s := startSandbox(t, &fakeDeployment{})
	if err := s.Checkpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "does not support checkpoints") {
		t.Fatalf("Checkpoint() error = %v", err)
	}
	if err := s.Restore(context.Background()); err == nil || !strings.Contains(err.Error(), "does not support checkpoints") {
		t.Fatalf("Restore() error = %v", err)
	}
}
