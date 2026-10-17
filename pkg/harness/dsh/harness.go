// Package dsh runs the released DeepSeek Harness headless application.
package dsh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
)

const (
	privateRoot = "/run/aries/dsh"
	// Match the per-stream evidence bound used by the other harnesses. The
	// archive budget includes all root/child files and tar metadata.
	maxEvidenceFileBytes   = 16 << 20
	maxSessionArchiveBytes = 64 << 20
)

const launcherScript = `#!/bin/sh
set -eu
umask 077
mkdir -p "$DSH_HOME/profiles/headless"
ln -s /opt/dsh/node_modules "$DSH_HOME/profiles/headless/node_modules"
ARIES_MODEL_API_KEY=$(cat /run/aries/dsh/model.key)
export ARIES_MODEL_API_KEY
exec dsh --profile headless --patch /run/aries/dsh/config.json -- --json -
`

type Options struct {
	Runtime harness.RuntimeOptions
	Common  harness.Options
}

type Manager struct {
	mu      sync.Mutex
	runtime *harness.Runtime
	options harness.Options
	active  *harness.Occurrence
}

func New(options Options) (*Manager, error) {
	runtime, err := harness.NewRuntime("DSH", options.Runtime)
	if err != nil {
		return nil, err
	}
	if options.Common.Mode != "" && options.Common.Mode != "agent" {
		return nil, errors.New("DSH supports agent mode only")
	}
	return &Manager{runtime: runtime, options: options.Common}, nil
}

func (manager *Manager) Close() error { return manager.runtime.Close() }

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtime.InUse() {
		return errors.New("DSH harness is already active")
	}
	if err := harness.ValidateRunID("DSH", request.RunID); err != nil {
		return err
	}
	if err := harness.ValidateTaskID("DSH", request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("DSH task timeout must not be negative")
	}
	if request.NoSandboxTools || len(request.Connectivity.MCPServers) != 0 {
		return errors.New("DSH task MCP/tool restrictions are not supported")
	}
	if err := harness.ValidateModel("DSH", request.Model); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(request.Endpoint.Address)
	if err != nil || request.Endpoint.Protocol != "ssh" || request.Endpoint.Username != "aries" || !core.ValidEndpointHost(host) {
		return errors.New("DSH requires its paired SSH endpoint")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber <= 0 || portNumber > 65535 {
		return errors.New("invalid DSH SSH port")
	}
	id, err := privatefiles.RandomID()
	if err != nil {
		return err
	}
	credentials := harness.NewCredentials("DSH")
	defer func() {
		if credentials != nil {
			credentials.Clear()
		}
	}()
	credentials.AddRedactions(manager.options.RedactEnv, manager.runtime.Options.APIKeyLookup)
	if ok, err := credentials.Load("model", request.Model.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("DSH API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = manager.runtime.Options.AgentTimeout
	}
	active := &harness.Occurrence{
		RunID: request.RunID, TaskID: request.TaskID, AttemptID: id,
		Name:        "aries-dsh-" + id,
		ArtifactDir: filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness"),
		Endpoint:    request.Endpoint, Model: request.Model,
		AgentTimeout: timeout, Credentials: credentials,
	}
	active.Artifacts = &harness.Artifacts{Directory: active.ArtifactDir}
	credentials = nil
	if err := manager.runtime.Own(active); err != nil {
		active.Credentials.Clear()
		return err
	}
	fail := func(err error) error {
		err = manager.runtime.Rollback(ctx, active, active.Credentials.RedactErr(err))
		if manager.runtime.InUse() {
			manager.active = active
		}
		return err
	}
	sshConfig := fmt.Sprintf("Host aries-sandbox\n HostName %s\n Port %d\n User aries\n UserKnownHostsFile %s/known_hosts\n IdentityFile none\n PubkeyAuthentication no\n PasswordAuthentication no\n KbdInteractiveAuthentication no\n", host, portNumber, privateRoot)
	files := map[string]harness.ArchiveFile{
		"run/aries/dsh/model.key": {Content: active.Credentials.Get("model"), Mode: 0600},
		// OpenSSH resolves its user config from getpwuid, not $HOME. The pinned
		// Node image runs as root; this remains private to the harness container.
		"root/.ssh/config":        {Content: []byte(sshConfig), Mode: 0600},
		"run/aries/dsh/launch.sh": {Content: []byte(launcherScript), Mode: 0500},
	}
	directories := []harness.ArchiveDirectory{
		{Name: "run/aries", Mode: 0700},
		{Name: "run/aries/dsh", Mode: 0700},
		{Name: "run/aries/dsh/home", Mode: 0700},
		{Name: "root/.ssh", Mode: 0700},
	}
	archive, err := harness.StageArchive(directories, files)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	runtimeRequest := deployment.Request{
		Name: active.Name, Image: manager.runtime.Options.Image,
		Workdir: privateRoot, Placement: request.Connectivity.Placement,
		CPU: request.CPU, MemoryMB: request.MemoryMB,
		Entrypoint: []string{"/bin/sh"}, Args: []string{"-c", "exec sleep infinity"},
		Env: []string{"HOME=" + privateRoot + "/home", "DSH_HOME=" + privateRoot + "/home"},
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "dsh-harness",
			"aries.component": "harness", "aries.run": request.RunID,
			"aries.task": request.TaskID, "aries.attempt": id,
		},
	}
	if err := manager.runtime.Start(ctx, active, runtimeRequest, archive, active.Credentials.Secrets()); err != nil {
		return fail(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.runtime.Options.StartTimeout)
	defer cancel()
	result, err := manager.runtime.Options.Deployment.Exec(readyCtx, active.ID, core.Command{Path: "/bin/cat", Args: []string{"/opt/aries/dsh-helper.sha256"}})
	if err != nil || result.ExitCode != 0 {
		return fail(errors.Join(errors.New("read pinned DSH helper digest"), err))
	}
	configuration, err := renderConfig(request.Model, manager.options, request.Endpoint, strings.TrimSpace(result.Stdout))
	if err != nil {
		return fail(err)
	}
	if bytes.Contains(configuration, active.Credentials.Get("model")) {
		return fail(errors.New("rendered DSH config contains the API-key value"))
	}
	if err := active.Artifacts.Write("config.json", active.Credentials.Redact(configuration)); err != nil {
		return fail(err)
	}
	archiveConfig, err := harness.StageArchive(nil, map[string]harness.ArchiveFile{"run/aries/dsh/config.json": {Content: configuration, Mode: 0600}})
	if err != nil {
		return fail(err)
	}
	defer clear(archiveConfig)
	if err := manager.runtime.Options.Deployment.UploadArchive(readyCtx, active.ID, "/", bytes.NewReader(archiveConfig)); err != nil {
		return fail(err)
	}
	result, err = manager.runtime.Options.Deployment.Exec(readyCtx, active.ID, core.Command{Path: "/usr/bin/ssh-keyscan", Args: []string{"-T", "5", "-p", port, host}})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		return fail(errors.Join(errors.New("collect DSH bridge host key"), err))
	}
	knownHosts, err := harness.StageArchive(nil, map[string]harness.ArchiveFile{"run/aries/dsh/known_hosts": {Content: []byte(result.Stdout), Mode: 0600}})
	if err != nil {
		return fail(err)
	}
	defer clear(knownHosts)
	if err := manager.runtime.Options.Deployment.UploadArchive(readyCtx, active.ID, "/", bytes.NewReader(knownHosts)); err != nil {
		return fail(err)
	}
	manager.active = active
	manager.runtime.Ready()
	return nil
}

func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	if err := manager.runtime.AdmitRun(instruction); err != nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed, Error: err.Error()}, err
	}
	active := manager.active.Snapshot()
	manager.mu.Unlock()
	defer active.Credentials.Clear()
	runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
	var stdout, stderr bytes.Buffer
	commandResult, runErr := manager.runtime.Options.Deployment.ExecStream(runCtx, active.ID, core.Command{
		Path: privateRoot + "/launch.sh", Dir: privateRoot,
		OutputLimitBytes: maxEvidenceFileBytes,
	}, strings.NewReader(instruction), &stdout, &stderr)
	if err := runCtx.Err(); err != nil {
		runErr = errors.Join(runErr, err)
	}
	cancel()
	final, protocolErr := parseOutcome(stdout.Bytes())
	if commandResult.ExitCode != 0 {
		runErr = errors.Join(runErr, fmt.Errorf("DSH exited with status %d", commandResult.ExitCode))
	}
	runErr = errors.Join(runErr, protocolErr)
	collectCtx, collectCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	defer collectCancel()
	for _, item := range []struct {
		name    string
		content []byte
	}{{"events.jsonl", stdout.Bytes()}, {"stderr.log", stderr.Bytes()}, {"instruction.txt", []byte(instruction)}} {
		runErr = errors.Join(runErr, active.Artifacts.Write(item.name, active.Credentials.Redact(item.content)))
	}
	runErr = errors.Join(runErr, manager.collectSessions(collectCtx, active))
	timing, _ := json.MarshalIndent(map[string]any{"started_at": started.UTC(), "finished_at": time.Now().UTC(), "exit_code": commandResult.ExitCode}, "", "  ")
	runErr = errors.Join(runErr, active.Artifacts.Write("outcome.json", timing))
	if runErr != nil {
		result := active.FailedResult(started, runErr)
		return result, active.Credentials.RedactErr(runErr)
	}
	return core.HarnessResult{
		Status:        core.StatusSucceeded,
		FinalResponse: string(active.Credentials.Redact([]byte(final))),
		Duration:      time.Since(started), LogPaths: active.Artifacts.Paths(),
	}, nil
}

// Native headless exit status alone is insufficient: require its final event
// after the final root turn ended as completed, with no pending tool calls.
func parseOutcome(content []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	root, turn, completed, finished := "", 0, false, false
	pending := map[string]bool{}
	final := ""
	for {
		var event struct {
			Type, Phase, SessionID, CallID, Text string
			Turn                                 int
			Reason                               struct{ Kind string }
			Truncated                            bool
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid DSH headless event stream: %w", err)
		}
		if finished {
			return "", errors.New("DSH emitted events after its final event")
		}
		switch event.Type {
		case "session":
			if root != "" || event.SessionID == "" {
				return "", errors.New("invalid DSH root session")
			}
			root = event.SessionID
		case "status":
			if event.Phase == "turn_start" {
				turn = event.Turn
				completed = false
			}
			if event.Phase == "turn_end" {
				completed = turn > 0 && event.Turn == turn && event.Reason.Kind == "completed" && !event.Truncated
			}
		case "tool_call":
			pending[event.CallID] = true
		case "tool_result":
			delete(pending, event.CallID)
		case "final":
			final = event.Text
			finished = true
		case "error":
			return "", errors.New("DSH reported a headless error")
		}
	}
	if root == "" || !completed || !finished || strings.TrimSpace(final) == "" || len(pending) != 0 {
		return "", errors.New("DSH lacks a complete final root turn or has pending tool calls")
	}
	return final, nil
}

func (manager *Manager) collectSessions(ctx context.Context, active *harness.Occurrence) error {
	archive, _, err := manager.runtime.Options.Deployment.DownloadArchive(ctx, active.ID, privateRoot+"/sessions")
	if err != nil {
		return fmt.Errorf("collect DSH durable sessions: %w", err)
	}
	defer archive.Close()
	limited := &io.LimitedReader{R: archive, N: maxSessionArchiveBytes + 1}
	reader := tar.NewReader(limited)
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if limited.N == 0 {
			return fmt.Errorf("DSH session archive exceeds %d bytes", maxSessionArchiveBytes)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(path.Clean(header.Name), "sessions/")
		if header.Typeflag != tar.TypeReg || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return errors.New("invalid DSH session archive path")
		}
		if header.Size < 0 || header.Size > maxEvidenceFileBytes {
			return fmt.Errorf("DSH session %q exceeds %d bytes", name, maxEvidenceFileBytes)
		}
		content, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if limited.N == 0 {
			return fmt.Errorf("DSH session archive exceeds %d bytes", maxSessionArchiveBytes)
		}
		if err != nil {
			return err
		}
		if int64(len(content)) != header.Size {
			return fmt.Errorf("read DSH session %q: %w", name, io.ErrUnexpectedEOF)
		}
		if err := active.Artifacts.Write("sessions/"+name, active.Credentials.Redact(content)); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return errors.New("DSH produced no durable session JSONL")
	}
	return nil
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
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	err := manager.runtime.Remove(cleanup, active)
	cancel()
	manager.mu.Lock()
	if active == nil || active.ID == "" {
		manager.active = nil
	}
	manager.runtime.FinishStop(err)
	manager.mu.Unlock()
	return err
}
