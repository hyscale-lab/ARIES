package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/monitor"
)

func TestVHiveExecutionAccessSharesOnlyExecutionSocketAndFIFOs(t *testing.T) {
	cfg := config.DeploymentConfig{Backend: "vhive", VHive: &config.VHiveDeploymentConfig{Socket: "/run/custom/containerd.sock", Namespace: "experiment", IODir: filepath.Join(t.TempDir(), "io")}}
	endpoint, mounts, err := VHiveExecutionAccess(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got vhive.ExecutionOptions
	if err = json.Unmarshal([]byte(endpoint), &got); err != nil {
		t.Fatal(err)
	}
	if got.Socket != cfg.VHive.Socket || got.Namespace != "experiment" || got.IODir != cfg.VHive.IODir || len(mounts) != 2 {
		t.Fatalf("endpoint=%v mounts=%v", got, mounts)
	}
	for _, mount := range mounts {
		if mount.Source != mount.Target || mount.ReadOnly {
			t.Fatalf("guest FIFO paths must survive handoff: %v", mount)
		}
	}
	if mounts[0].Source != got.Socket || mounts[1].Source != got.IODir {
		t.Fatalf("mounts=%v", mounts)
	}
	_, err = (vhiveResources{}).Sample(t.Context())
	if !errors.Is(err, monitor.ErrUnsupported) {
		t.Fatalf("VM metrics must not be reported as zero: %v", err)
	}
}

type taskEnvironmentStub struct {
	deployment.TaskEnvironment
	starts int
}

func (e *taskEnvironmentStub) Start(context.Context, deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	e.starts++
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: "run-network"}}, nil
}
func TestVHiveTaskEnvironmentRejectsUnroutableBenchmarkServices(t *testing.T) {
	base := &taskEnvironmentStub{}
	e := VHiveTaskEnvironment(base)
	r := deployment.TaskEnvironmentRequest{}
	r.Environment.Services.SearchPort = 8080
	if _, err := e.Start(t.Context(), r); err == nil || base.starts != 0 {
		t.Fatal("admitted an unroutable service")
	}
	r.Environment.Services.SearchPort = 0
	got, err := e.Start(t.Context(), r)
	if err != nil || base.starts != 1 || got.Placement.AttachmentID != "run-network" {
		t.Fatalf("attachment=%v err=%v", got, err)
	}
}
