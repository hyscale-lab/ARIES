// Package codex runs the pinned upstream Codex CLI in a private runtime.
// Task tools use the bridge's native remote exec-server environment over SSH.
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const maxOutputBytes = 16 << 20

var (
	runtimeEntrypoint = []string{"/bin/sh"}
	runtimeCommand    = []string{"-c", "exec sleep infinity"}
)

type Options struct {
	Runtime      harness.RuntimeOptions
	CodexPath    string
	CodexVersion string
	// CABundlePath overrides the host system trust bundle for HTTPS endpoints.
	// HTTP endpoints never read it. An empty path uses fixed Linux system paths.
	CABundlePath string
}

type Manager struct {
	runtime                     *harness.Runtime
	codexSource, caBundleSource string
	newID                       func() (string, error)

	mu     sync.Mutex
	active *harness.Occurrence
}

var _ runner.AgentHarness = (*Manager)(nil)

// New validates host-local options without contacting the deployment.
func New(options Options) (*Manager, error) {
	if options.CodexVersion != supportedVersion {
		return nil, fmt.Errorf("Codex native SSH integration requires version %s", supportedVersion)
	}
	if strings.TrimSpace(options.CodexPath) == "" {
		return nil, errors.New("Codex executable is required")
	}
	source, err := filepath.Abs(options.CodexPath)
	if err != nil {
		return nil, err
	}
	caBundleSource := ""
	if options.CABundlePath != "" {
		if strings.TrimSpace(options.CABundlePath) == "" {
			return nil, errors.New("Codex CA bundle path must not be blank")
		}
		caBundleSource, err = filepath.Abs(options.CABundlePath)
		if err != nil {
			return nil, err
		}
	}
	runtime, err := harness.NewRuntime("Codex", options.Runtime)
	if err != nil {
		return nil, err
	}
	return &Manager{runtime: runtime, codexSource: source, caBundleSource: caBundleSource, newID: privatefiles.RandomID}, nil
}

// Close releases the deployment transport after lifecycle cleanup.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	return manager.runtime.Close()
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtime.InUse() {
		return errors.New("Codex harness is already active")
	}
	if err := harness.ValidateRunID("Codex", request.RunID); err != nil {
		return err
	}
	if err := harness.ValidateTaskID("Codex", request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("Codex task timeout must not be negative")
	}
	configuration, err := renderConfig(request.Model)
	if err != nil {
		return err
	}
	environments, err := renderEnvironments(request.Endpoint, request.Connectivity.Placement)
	if err != nil {
		return err
	}
	credentials := harness.NewCredentials("Codex")
	defer func() {
		if credentials != nil {
			credentials.Clear()
		}
	}()
	if ok, err := credentials.Load("model", request.Model.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil || !ok {
		return errors.New("Codex model key is missing, oversized, or contains NUL or line breaks")
	}
	id, err := manager.newID()
	if err != nil {
		return fmt.Errorf("generate Codex harness ID: %w", err)
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.runtime.Options.AgentTimeout
	}
	deploymentRequest := deployment.Request{
		Name: "aries-codex-" + id, Image: manager.runtime.Options.Image,
		Placement: request.Connectivity.Placement, CPU: request.CPU, MemoryMB: request.MemoryMB,
		Env:        []string{"HOME=" + codexHome, "CODEX_HOME=" + codexHome, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Entrypoint: append([]string(nil), runtimeEntrypoint...), Args: append([]string(nil), runtimeCommand...),
		NoNewPrivileges: true, DropCapabilities: true,
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "codex-harness", "aries.component": "harness",
			"aries.run": request.RunID, "aries.task": request.TaskID, "aries.attempt": id,
		},
	}
	key := credentials.Get("model")
	if bytes.Contains(configuration, key) || bytes.Contains(environments, key) {
		return errors.New("Codex credential overlaps its rendered configuration")
	}
	active := &harness.Occurrence{
		RunID: request.RunID, TaskID: request.TaskID, AttemptID: id, Name: deploymentRequest.Name, DeploymentRequest: deploymentRequest,
		ArtifactDir: filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness"), Endpoint: request.Endpoint, Model: request.Model, AgentTimeout: agentTimeout,
		Credentials: credentials,
	}
	active.Artifacts = &harness.Artifacts{Directory: active.ArtifactDir}
	credentials = nil
	if err := manager.runtime.Own(active); err != nil {
		active.Credentials.Clear()
		return err
	}
	fail := func(primary error) error {
		err := manager.runtime.Rollback(ctx, active, active.Credentials.RedactErr(primary))
		if manager.runtime.InUse() {
			manager.active = active
		}
		return err
	}
	if err := privatefiles.EnsureDirectory(active.ArtifactDir); err != nil {
		return fail(fmt.Errorf("create Codex artifact directory: %w", err))
	}
	for _, artifact := range []namedFile{{"config.toml", configuration}, {"environments.toml", environments}} {
		if err := active.Artifacts.Write(artifact.name, artifact.data); err != nil {
			return fail(fmt.Errorf("retain Codex configuration: %w", err))
		}
	}
	archive, err := manager.runtimeArchive(active, configuration, environments)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	startCtx, cancel := context.WithTimeout(ctx, manager.runtime.Options.StartTimeout)
	defer cancel()
	if err := manager.runtime.Start(startCtx, active, deploymentRequest, archive, active.Credentials.Secrets()); err != nil {
		return fail(err)
	}
	version, err := manager.runtime.Options.Deployment.Exec(startCtx, active.ID, core.Command{Path: codexPath, Args: []string{"--version"}, Dir: "/", OutputLimitBytes: maxOutputBytes})
	if err != nil {
		return fail(fmt.Errorf("check staged Codex version: %w", err))
	}
	if version.ExitCode != 0 || strings.TrimSpace(version.Stdout) != "codex-cli "+supportedVersion {
		return fail(errors.New("staged Codex does not report the pinned version"))
	}
	manager.active = active
	manager.runtime.Ready()
	manager.runtime.Options.Logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.TaskID, "container": active.Name}).Info("Codex harness started")
	return nil
}

func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	if manager.active != nil && bytes.Contains([]byte(instruction), manager.active.Credentials.Get("model")) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Codex task instruction contains the model credential")
	}
	if err := manager.runtime.AdmitRun(instruction); err != nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, err
	}
	active := manager.active.Snapshot()
	manager.mu.Unlock()
	defer active.Credentials.Clear()
	runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
	output, runErr := manager.runtime.Options.Deployment.Exec(runCtx, active.ID, core.Command{
		Path: agentWrapperPath, Args: []string{"exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--cd", active.Endpoint.Workdir, "--", instruction},
		Dir: active.Endpoint.Workdir, OutputLimitBytes: maxOutputBytes,
	})
	cancel()
	stdout, stderr := active.Credentials.Redact([]byte(output.Stdout)), active.Credentials.Redact([]byte(output.Stderr))
	if runErr == nil && output.ExitCode != 0 {
		runErr = fmt.Errorf("Codex exec exited with status %d", output.ExitCode)
	}
	final, trajectoryErr := finalResponse(stdout)
	if runErr == nil {
		runErr = trajectoryErr
	}
	// Codex's native rollout records the session, including model inputs and
	// tool outputs that the --json event stream summarizes. A failed exec may
	// end before Codex creates one.
	copyCtx, cancelCopy := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	rollouts, rolloutErr := manager.copyRollouts(copyCtx, active.ID)
	cancelCopy()
	if rolloutErr == nil && len(rollouts) == 0 && runErr == nil {
		rolloutErr = errors.New("Codex wrote no rollout")
	}
	if rolloutErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("retain Codex rollout: %w", rolloutErr))
	}
	artifacts := []namedFile{{"trajectory.jsonl", stdout}, {"stderr.log", stderr}}
	for _, rollout := range rollouts {
		artifacts = append(artifacts, namedFile{filepath.Join("sessions", rollout.name), active.Credentials.Redact(rollout.data)})
	}
	for _, artifact := range artifacts {
		if err := active.Artifacts.Write(artifact.name, artifact.data); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain Codex output: %w", err))
		}
	}
	if runErr != nil {
		runErr = active.Credentials.RedactErr(runErr)
		result := active.FailedResult(started, runErr)
		result.FinalResponse = final
		return result, runErr
	}
	return core.HarnessResult{Status: core.StatusSucceeded, FinalResponse: final, Duration: time.Since(started), LogPaths: active.Artifacts.Paths()}, nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	attempt, ownsCleanup := manager.runtime.BeginStop()
	if !ownsCleanup {
		manager.mu.Unlock()
		return attempt.Wait(ctx)
	}
	active := manager.active
	manager.mu.Unlock()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	err := manager.runtime.Remove(cleanupCtx, active)
	cancel()
	if err != nil && active != nil {
		err = active.Credentials.RedactErr(err)
	}
	manager.mu.Lock()
	if active == nil || active.ID == "" {
		manager.active = nil
	}
	manager.runtime.FinishStop(err)
	manager.mu.Unlock()
	return err
}
