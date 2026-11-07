package bridge

import (
	"errors"
	"os"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/bridge/grpc"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/replay"
)

// NewService takes ownership of the supplied run-scoped runtime transport.
func NewService(cfg config.Config, runID, outputRoot string, placement core.RuntimePlacement, runtime deployment.Runtime, launch managed.LaunchSpec) (*managed.Service, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	client, err := ClientConfig(cfg.Bridge.Type, filepath.Dir(executable))
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	service, err := managed.NewService(managed.Options{Runtime: runtime, Launch: launch, Client: client, RunID: runID, Placement: placement, OutputDir: outputRoot, BridgeType: cfg.Bridge.Type, RetainRawLog: cfg.Bridge.RetainBridgeRawLog(), BenchmarkType: cfg.Benchmark.Type, BenchmarkRevisions: BenchmarkRevisions(cfg)})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return service, nil
}

// LoadReplaySource binds source validation to the selected catalog task and pins.
func LoadReplaySource(cfg config.Config, task core.Task) (replay.Plan, error) {
	return grpc.LoadReplaySource(cfg.Replay.Source, task, cfg.Benchmark.Type, BenchmarkRevisions(cfg))
}

// BenchmarkRevisions returns the same pins used by live bridge evidence.
func BenchmarkRevisions(cfg config.Config) map[string]string {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		return map[string]string{"revision": cfg.Versions.TerminalBench2.Revision}
	case "deepresearchbench":
		return map[string]string{"revision": cfg.Versions.DeepResearchBench.Revision}
	case "sweatlasqa":
		return map[string]string{"revision": cfg.Versions.SWEAtlas.Revision}
	case "swebenchpro":
		return map[string]string{"dataset_revision": cfg.Versions.SWEbenchPro.DatasetRevision, "evaluator_revision": cfg.Versions.SWEbenchPro.EvaluatorRevision}
	default:
		return nil
	}
}
