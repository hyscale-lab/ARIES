package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

// Unused provider operations remain unavailable so an unexpected runtime call fails.
type runtimeDeployment struct {
	deployment.Deployment
	createErr      error
	stopErr        error
	calls          []string
	stopContextErr error
	stopBounded    bool
}

func (d *runtimeDeployment) Create(context.Context, deployment.Request) (string, error) {
	d.calls = append(d.calls, "create")
	return "owned-runtime", d.createErr
}
func (d *runtimeDeployment) UploadArchive(_ context.Context, id, path string, archive io.Reader) error {
	d.calls = append(d.calls, "upload")
	if id != "owned-runtime" || path != "/" {
		return errors.New("unexpected staging target")
	}
	_, err := io.ReadAll(archive)
	return err
}
func (d *runtimeDeployment) Validate(_ context.Context, id string, _ deployment.Request, secrets [][]byte) error {
	d.calls = append(d.calls, "validate")
	if id != "owned-runtime" || len(secrets) != 1 || string(secrets[0]) != "private-key" {
		return errors.New("missing identity or validation credentials")
	}
	return nil
}
func (d *runtimeDeployment) Start(context.Context, string) error {
	d.calls = append(d.calls, "start")
	return nil
}
func (d *runtimeDeployment) Stop(ctx context.Context, id string) error {
	d.calls = append(d.calls, "stop")
	d.stopContextErr = ctx.Err()
	_, d.stopBounded = ctx.Deadline()
	if id != "owned-runtime" {
		return errors.New("lost cleanup identity")
	}
	return d.stopErr
}

func runtimeFixture(t *testing.T, provider *runtimeDeployment) (*Runtime, *Occurrence) {
	t.Helper()
	runtime, err := NewRuntime("test", RuntimeOptions{Deployment: provider, Image: "test/runtime:1", OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	credentials := NewCredentials("test")
	credentials.Set("model", []byte("private-key"))
	occurrence := &Occurrence{ArtifactDir: filepath.Join(runtime.Options.OutputDir, "task", "harness"), Credentials: credentials}
	if err := runtime.Own(occurrence); err != nil {
		t.Fatal(err)
	}
	return runtime, occurrence
}

func TestPartialCreateRollbackRetainsOwnershipUntilConfirmed(t *testing.T) {
	createErr, stopErr := errors.New("partial allocation"), errors.New("absence unconfirmed")
	provider := &runtimeDeployment{createErr: createErr, stopErr: stopErr}
	runtime, occurrence := runtimeFixture(t, provider)
	occurrence.Artifacts = &Artifacts{Directory: occurrence.ArtifactDir}
	if err := occurrence.Artifacts.Write("config", []byte("private config")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := runtime.Start(ctx, occurrence, deployment.Request{}, nil, occurrence.Credentials.Secrets())
	if !errors.Is(err, createErr) || occurrence.ID != "owned-runtime" {
		t.Fatalf("partial identity lost: %q, %v", occurrence.ID, err)
	}
	cancel()
	rollbackErr := runtime.Rollback(ctx, occurrence, err)
	if !errors.Is(rollbackErr, createErr) || !errors.Is(rollbackErr, stopErr) {
		t.Fatalf("rollback lost failure causes: %v", rollbackErr)
	}
	if provider.stopContextErr != nil || !provider.stopBounded {
		t.Fatal("rollback did not use fresh bounded cleanup context")
	}
	if !runtime.InUse() || occurrence.ID != "owned-runtime" || string(occurrence.Credentials.Get("model")) != "private-key" {
		t.Fatal("failed rollback released live ownership or credentials")
	}
	if _, err := os.Stat(occurrence.ArtifactDir); err != nil {
		t.Fatalf("failed rollback lost evidence: %v", err)
	}
	if err := runtime.AdmitRun("task"); err == nil {
		t.Fatal("admitted a partially started runtime")
	}
	alias := occurrence.Credentials.Get("model")
	provider.stopErr = nil
	if err := runtime.Rollback(ctx, occurrence, createErr); !errors.Is(err, createErr) || errors.Is(err, stopErr) {
		t.Fatalf("retry result: %v", err)
	}
	if runtime.InUse() || occurrence.ID != "" || occurrence.Credentials.Get("model") != nil || !bytes.Equal(alias, make([]byte, len(alias))) {
		t.Fatal("confirmed removal retained resources or credential bytes")
	}
	if _, err := os.Stat(occurrence.ArtifactDir); !os.IsNotExist(err) {
		t.Fatalf("successful rollback retained partial artifacts: %v", err)
	}
	if !reflect.DeepEqual(provider.calls, []string{"create", "stop", "stop"}) {
		t.Fatalf("continued startup after partial allocation: %v", provider.calls)
	}
}

func TestFailedStopPreventsFirstRunAndAllowsCleanupRetry(t *testing.T) {
	provider := &runtimeDeployment{stopErr: errors.New("absence unconfirmed")}
	runtime, occurrence := runtimeFixture(t, provider)
	if err := runtime.Start(context.Background(), occurrence, deployment.Request{}, []byte("archive"), occurrence.Credentials.Secrets()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AdmitRun("task"); err == nil {
		t.Fatal("runtime was ready before native readiness")
	}
	runtime.Ready()
	attempt, owner := runtime.BeginStop()
	if !owner {
		t.Fatal("stop did not acquire cleanup ownership")
	}
	stopErr := runtime.Remove(context.Background(), occurrence)
	runtime.FinishStop(stopErr)
	if err := attempt.Wait(context.Background()); !errors.Is(err, provider.stopErr) {
		t.Fatalf("stop result: %v", err)
	}
	if err := runtime.AdmitRun("task"); err == nil {
		t.Fatal("failed cleanup admitted a new run")
	}
	if !runtime.InUse() || occurrence.ID == "" || len(occurrence.Credentials.Get("model")) == 0 {
		t.Fatal("failed stop lost cleanup ownership")
	}
	provider.stopErr = nil
	_, owner = runtime.BeginStop()
	if !owner {
		t.Fatal("cleanup retry did not acquire ownership")
	}
	err := runtime.Remove(context.Background(), occurrence)
	runtime.FinishStop(err)
	if err != nil || runtime.InUse() || occurrence.ID != "" || occurrence.Credentials.Get("model") != nil {
		t.Fatalf("cleanup retry incomplete: %v", err)
	}
	if !reflect.DeepEqual(provider.calls, []string{"create", "validate", "upload", "validate", "start", "stop", "stop"}) {
		t.Fatalf("lifecycle order: %v", provider.calls)
	}
}
