package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// workdirProbes bounds how many images are probed at once.
const workdirProbes = 8

// resolveWorkdirs finds, once per run, the working directory of each task
// image that names none, so its tasks' harnesses can start alongside their
// sandboxes. It starts a sandbox from the image, as a task would, and reads the
// directory that sandbox resolved. An image whose probe fails is left out, and
// its tasks start their harness once the sandbox has started.
func resolveWorkdirs(ctx context.Context, cfg config.Config, runID, outputRoot string, tasks []core.Task, wiring Wiring, logger *logrus.Logger) map[runner.WorkdirKey]string {
	var keys []runner.WorkdirKey
	seen := make(map[runner.WorkdirKey]bool)
	for _, task := range tasks {
		key := runner.WorkdirKey{Image: task.Environment.Image, ExecUser: task.Environment.ExecUser}
		if task.Environment.Workdir == "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 || wiring.NewSandbox == nil {
		return nil
	}
	started := time.Now()
	workdirs := make(map[runner.WorkdirKey]string, len(keys))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, workdirProbes)
	for i, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			workdir, err := probeWorkdir(ctx, cfg, runID, outputRoot, fmt.Sprintf("workdir-probe-%d", i+1), key, wiring, logger)
			if err != nil {
				logger.WithContext(ctx).WithError(err).WithField("image", key.Image).Warn("task image working directory unresolved; its harnesses start after their sandboxes")
				return
			}
			mu.Lock()
			workdirs[key] = workdir
			mu.Unlock()
		}()
	}
	wg.Wait()
	logger.WithContext(ctx).WithFields(logrus.Fields{"images": len(keys), "resolved": len(workdirs), "duration": time.Since(started).String()}).Info("task image working directories resolved")
	return workdirs
}

func probeWorkdir(ctx context.Context, cfg config.Config, runID, outputRoot, probeID string, key runner.WorkdirKey, wiring Wiring, logger *logrus.Logger) (string, error) {
	instance, err := wiring.NewSandbox(cfg, filepath.Join(outputRoot, "infrastructure", "workdirs"), runID, probeID, nil, logger)
	if err != nil {
		return "", err
	}
	defer func() {
		if instance.Resources != nil {
			_ = instance.Resources.Close()
		}
		if instance.Close != nil {
			_ = instance.Close()
		}
	}()
	live, err := instance.Sandbox.Start(ctx, core.SandboxRequest{RunID: runID, TaskID: probeID, Environment: core.Environment{Image: key.Image, ExecUser: key.ExecUser}})
	if err != nil {
		return "", err
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	stopErr := instance.Sandbox.Stop(stopCtx, live)
	resolved, ok := live.(interface{ Workdir() string })
	if !ok {
		return "", errors.Join(errors.New("sandbox does not report its working directory"), stopErr)
	}
	if stopErr != nil {
		return "", stopErr
	}
	return resolved.Workdir(), nil
}
