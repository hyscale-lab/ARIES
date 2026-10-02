package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

func TestConstructionClosesSelectedDependenciesOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		gpu          []int
	}{
		{name: "sandbox options"},
		{name: "GPU monitor", output: t.TempDir(), gpu: []int{-1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transportErr := errors.New("transport close")
			resourceErr := errors.New("resource close")
			transport := &closeDeployment{err: transportErr}
			resources := &wiringResourceSource{closeErr: resourceErr}
			_, err := New(tc.output, "task", tc.gpu, nil, transport, func() deployment.TaskEnvironment { return nil }, resources)
			if err == nil || !errors.Is(err, transportErr) || !errors.Is(err, resourceErr) {
				t.Fatalf("construction/cleanup error=%v", err)
			}
			if transport.closes != 1 || resources.closes != 1 {
				t.Fatalf("closes: transport=%d resources=%d", transport.closes, resources.closes)
			}
		})
	}
}

func TestConstructionTransfersResourceOwnership(t *testing.T) {
	transport := &closeDeployment{}
	resources := &wiringResourceSource{}
	instance, err := New(t.TempDir(), "task", nil, nil, transport, func() deployment.TaskEnvironment { return nil }, resources)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Resources != resources || instance.BridgeListen == nil {
		t.Fatal("missing selected resource source or bridge resolver")
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if transport.closes != 1 || resources.closes != 0 {
		t.Fatalf("sandbox close must leave observer-owned resources open: transport=%d resources=%d", transport.closes, resources.closes)
	}
	if err := instance.Resources.Close(); err != nil {
		t.Fatal(err)
	}
}

type closeDeployment struct {
	deployment.Deployment
	closes int
	err    error
}

func (d *closeDeployment) Close() error { d.closes++; return d.err }

type wiringResourceSource struct {
	readings []core.ResourceReading
	closeErr error
	closes   int
}

func (s *wiringResourceSource) Sample(context.Context) ([]core.ResourceReading, error) {
	return s.readings, nil
}
func (s *wiringResourceSource) Close() error { s.closes++; return s.closeErr }

func TestCombinedResourceSourceSamplesAndClosesBothSources(t *testing.T) {
	firstErr := errors.New("first close")
	secondErr := errors.New("second close")
	container := &wiringResourceSource{readings: []core.ResourceReading{{RuntimeID: "container"}}, closeErr: firstErr}
	gpu := &wiringResourceSource{readings: []core.ResourceReading{{RuntimeID: "gpu"}}, closeErr: secondErr}
	source := &combinedResourceSource{container: container, gpu: gpu}
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 || readings[0].RuntimeID != "container" || readings[1].RuntimeID != "gpu" {
		t.Fatalf("readings = %#v", readings)
	}
	if err := source.Close(); !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("close error = %v", err)
	}
	if container.closes != 1 || gpu.closes != 1 {
		t.Fatalf("closes = container %d gpu %d", container.closes, gpu.closes)
	}
}
