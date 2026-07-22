package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	defaultDockerSocket      = "/var/run/docker.sock"
	defaultSampleInterval    = time.Second
	defaultRequestTimeout    = 5 * time.Second
	defaultStopTimeout       = 10 * time.Second
	defaultMaxSamplesPerTask = 172800
	defaultMaxFileBytes      = int64(128 << 20)
	maxIdentityLength        = 128
	transientValidationGrace = 20 * time.Second
)

// Options are the explicit inputs to one run-scoped Recorder.
type Options struct {
	RunID             string
	TaskIDs           []string
	OutputDir         string
	DockerSocket      string
	Interval          time.Duration
	RequestTimeout    time.Duration
	StopTimeout       time.Duration
	MaxSamplesPerTask int
	MaxFileBytes      int64
	Logger            *slog.Logger
}

// Recorder observes ARIES-owned task and harness containers without controlling them.
type Recorder struct {
	runID             string
	taskIDs           []string
	taskSet           map[string]struct{}
	outputDir         string
	interval          time.Duration
	requestTimeout    time.Duration
	stopTimeout       time.Duration
	maxSamplesPerTask int
	maxFileBytes      int64
	logger            *slog.Logger
	engine            *engineClient
	artifactOps       artifactOperations
	now               func() time.Time
	writeIndex        func(string, Index) error

	mu            sync.Mutex
	starting      bool
	started       bool
	startTime     time.Time
	stopTime      time.Time
	artifacts     map[string]*taskArtifact
	sampleCancel  context.CancelFunc
	sampleDone    chan struct{}
	backgroundErr error
	filesClosed   bool
	stopAttempt   *stopAttempt
	completed     bool
	reports       map[string]core.ObserverResult
	sampleStates  map[string]containerSampleState
}

type stopAttempt struct {
	done     chan struct{}
	finished bool
	reports  map[string]core.ObserverResult
	err      error
}

type containerSampleState struct {
	hasValidSample bool
	invalidSince   time.Time
	validationErr  error
}

type pendingValidation struct {
	containerID string
	err         error
}

// New validates configuration without contacting Docker or creating artifacts.
func New(options Options) (*Recorder, error) {
	if err := validateIdentity("run", options.RunID); err != nil {
		return nil, err
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("monitor requires at least one task ID")
	}
	tasks := append([]string(nil), options.TaskIDs...)
	sort.Strings(tasks)
	taskSet := make(map[string]struct{}, len(tasks))
	for _, taskID := range tasks {
		if err := validateIdentity("task", taskID); err != nil {
			return nil, err
		}
		if _, duplicate := taskSet[taskID]; duplicate {
			return nil, fmt.Errorf("monitor task ID %q is repeated", taskID)
		}
		taskSet[taskID] = struct{}{}
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("monitor output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve monitor output directory: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	dockerSocket, err := filepath.Abs(options.DockerSocket)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker socket: %w", err)
	}
	if options.Interval == 0 {
		options.Interval = defaultSampleInterval
	}
	if options.Interval <= 0 {
		return nil, errors.New("monitor sample interval must be positive")
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.RequestTimeout <= 0 {
		return nil, errors.New("monitor request timeout must be positive")
	}
	if options.StopTimeout == 0 {
		options.StopTimeout = defaultStopTimeout
	}
	if options.StopTimeout <= 0 {
		return nil, errors.New("monitor stop timeout must be positive")
	}
	if options.MaxSamplesPerTask == 0 {
		options.MaxSamplesPerTask = defaultMaxSamplesPerTask
	}
	if options.MaxSamplesPerTask < 1 || options.MaxSamplesPerTask > 10_000_000 {
		return nil, errors.New("monitor sample bound must be between 1 and 10000000")
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = defaultMaxFileBytes
	}
	if options.MaxFileBytes < 4096 || options.MaxFileBytes > 1<<30 {
		return nil, errors.New("monitor file byte bound must be between 4096 and 1073741824")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	engine, err := newEngineClient(dockerSocket)
	if err != nil {
		return nil, fmt.Errorf("initialize Docker client: %w", err)
	}
	return &Recorder{
		runID:             options.RunID,
		taskIDs:           tasks,
		taskSet:           taskSet,
		outputDir:         outputDir,
		interval:          options.Interval,
		requestTimeout:    options.RequestTimeout,
		stopTimeout:       options.StopTimeout,
		maxSamplesPerTask: options.MaxSamplesPerTask,
		maxFileBytes:      options.MaxFileBytes,
		logger:            options.Logger,
		engine:            engine,
		artifactOps:       defaultArtifactOperations(),
		now:               time.Now,
		writeIndex:        writeIndexExact,
		sampleStates:      make(map[string]containerSampleState),
	}, nil
}

// Start performs synchronous discovery and an immediate sample before starting
// a cancellation-isolated sampler. A failed partial start removes its artifacts.
func (recorder *Recorder) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	recorder.mu.Lock()
	if recorder.started || recorder.starting {
		recorder.mu.Unlock()
		return errors.New("monitor recorder is already started")
	}
	if recorder.completed {
		recorder.mu.Unlock()
		return errors.New("monitor recorder has already stopped")
	}
	recorder.starting = true
	recorder.mu.Unlock()

	artifacts, monitorRoot, rootCreated, err := prepareArtifacts(recorder.outputDir, recorder.taskIDs, recorder.artifactOps)
	if err != nil {
		recorder.engine.closeIdleConnections()
		recorder.finishFailedStart()
		return err
	}
	startedAt := recorder.now().UTC()
	recorder.mu.Lock()
	recorder.artifacts = artifacts
	recorder.startTime = startedAt
	recorder.mu.Unlock()

	startFailure := func(cause error) error {
		recorder.engine.closeIdleConnections()
		rollbackErr := removePartialArtifacts(artifacts, monitorRoot, rootCreated, recorder.artifactOps)
		recorder.mu.Lock()
		recorder.artifacts = nil
		recorder.startTime = time.Time{}
		recorder.sampleStates = make(map[string]containerSampleState)
		recorder.starting = false
		recorder.mu.Unlock()
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("roll back partial monitor start: %w", rollbackErr))
		}
		return cause
	}
	if err := recorder.sample(ctx, 0, startedAt); err != nil {
		return startFailure(err)
	}
	if err := ctx.Err(); err != nil {
		return startFailure(err)
	}
	base := context.WithoutCancel(ctx)
	sampleContext, cancel := context.WithCancel(base)
	done := make(chan struct{})
	recorder.mu.Lock()
	recorder.sampleCancel = cancel
	recorder.sampleDone = done
	recorder.starting = false
	recorder.started = true
	recorder.mu.Unlock()
	go recorder.sampleLoop(sampleContext, done)
	recorder.logger.InfoContext(base, "resource monitoring started", "run", recorder.runID, "tasks", len(recorder.taskIDs))
	return nil
}

func (recorder *Recorder) finishFailedStart() {
	recorder.mu.Lock()
	recorder.starting = false
	recorder.mu.Unlock()
}

func (recorder *Recorder) sampleLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(recorder.interval)
	defer ticker.Stop()
	second := uint64(1)
	for {
		select {
		case <-ctx.Done():
			return
		case sampleTime := <-ticker.C:
			if err := recorder.sample(ctx, second, sampleTime.UTC()); err != nil {
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return
				}
				recorder.mu.Lock()
				if recorder.backgroundErr == nil {
					recorder.backgroundErr = err
				}
				recorder.mu.Unlock()
				return
			}
			second++
		}
	}
}

func (recorder *Recorder) sample(ctx context.Context, second uint64, sampleTime time.Time) error {
	requestCtx, cancel := requestContext(ctx, recorder.requestTimeout)
	containers, err := recorder.engine.discover(requestCtx, recorder.runID, recorder.taskSet)
	cancel()
	if err != nil {
		return err
	}
	recorder.reconcileSampleStates(containers)
	for _, container := range containers {
		requestCtx, cancel = requestContext(ctx, recorder.requestTimeout)
		measurement, sampleErr := recorder.engine.stats(requestCtx, container)
		cancel()
		if errors.Is(sampleErr, errContainerGone) {
			recorder.clearSampleState(container.ID)
			continue
		}
		if sampleErr != nil {
			var validationFailure *statsValidationError
			if errors.As(sampleErr, &validationFailure) && recorder.deferTransientValidation(container.ID, sampleErr) {
				continue
			}
			return sampleErr
		}
		artifact := recorder.artifacts[measurement.taskID]
		if artifact == nil {
			return fmt.Errorf("sampled unexpected task %q", measurement.taskID)
		}
		sample := ResourceSample{
			Second:           second,
			Time:             formatArtifactTime(sampleTime),
			TaskID:           measurement.taskID,
			Component:        measurement.kind,
			ContainerID:      measurement.id,
			ContainerName:    measurement.name,
			CPUPercent:       measurement.cpu,
			MemoryBytes:      measurement.memory,
			MemoryLimitBytes: measurement.memLimit,
		}
		if err := artifact.appendSample(sample, recorder.maxSamplesPerTask, recorder.maxFileBytes); err != nil {
			return fmt.Errorf("record monitor sample for task %s: %w", measurement.taskID, err)
		}
		recorder.recordValidSample(container.ID)
	}
	return nil
}

func (recorder *Recorder) reconcileSampleStates(containers []listedContainer) {
	present := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		present[container.ID] = struct{}{}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for containerID := range recorder.sampleStates {
		if _, ok := present[containerID]; !ok {
			delete(recorder.sampleStates, containerID)
		}
	}
}

func (recorder *Recorder) recordValidSample(containerID string) {
	recorder.mu.Lock()
	state := recorder.sampleStates[containerID]
	state.hasValidSample = true
	state.invalidSince = time.Time{}
	state.validationErr = nil
	recorder.sampleStates[containerID] = state
	recorder.mu.Unlock()
}

func (recorder *Recorder) deferTransientValidation(containerID string, validationErr error) bool {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	state := recorder.sampleStates[containerID]
	if !state.hasValidSample {
		return false
	}
	now := recorder.now()
	if state.invalidSince.IsZero() {
		state.invalidSince = now
		state.validationErr = validationErr
		recorder.sampleStates[containerID] = state
		return true
	}
	elapsed := now.Sub(state.invalidSince)
	return elapsed >= 0 && elapsed < transientValidationGrace
}

func (recorder *Recorder) clearSampleState(containerID string) {
	recorder.mu.Lock()
	delete(recorder.sampleStates, containerID)
	recorder.mu.Unlock()
}

func (recorder *Recorder) pendingValidations() []pendingValidation {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	pending := make([]pendingValidation, 0, len(recorder.sampleStates))
	for containerID, state := range recorder.sampleStates {
		if !state.invalidSince.IsZero() && state.validationErr != nil {
			pending = append(pending, pendingValidation{containerID: containerID, err: state.validationErr})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].containerID < pending[j].containerID })
	return pending
}

func (recorder *Recorder) reconcilePendingValidations() error {
	pending := recorder.pendingValidations()
	if len(pending) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recorder.requestTimeout)
	containers, err := recorder.engine.discover(ctx, recorder.runID, recorder.taskSet)
	cancel()
	if err != nil {
		failures := make([]error, 0, len(pending)+1)
		for _, validation := range pending {
			failures = append(failures, validation.err)
		}
		failures = append(failures, fmt.Errorf("reconcile pending monitor validation: %w", err))
		return errors.Join(failures...)
	}
	present := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		present[container.ID] = struct{}{}
	}
	var failures []error
	for _, validation := range pending {
		if _, ok := present[validation.containerID]; ok {
			failures = append(failures, validation.err)
			continue
		}
		recorder.clearSampleState(validation.containerID)
	}
	return errors.Join(failures...)
}

// Stop stops sampling independently of the caller's cancellation, finalizes
// private artifacts, and returns a cached exact report after success.
func (recorder *Recorder) Stop(ctx context.Context) (map[string]core.ObserverResult, error) {
	recorder.mu.Lock()
	if !recorder.started {
		starting := recorder.starting
		recorder.mu.Unlock()
		if !starting {
			recorder.engine.closeIdleConnections()
		}
		return nil, errors.New("monitor recorder is not started")
	}
	if recorder.completed {
		reports := cloneReports(recorder.reports)
		recorder.mu.Unlock()
		return reports, nil
	}
	attempt := recorder.stopAttempt
	if attempt == nil || attempt.finished {
		attempt = &stopAttempt{done: make(chan struct{})}
		recorder.stopAttempt = attempt
		go recorder.finishStop(attempt)
	}
	recorder.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-attempt.done:
		return cloneReports(attempt.reports), attempt.err
	}
}

func (recorder *Recorder) finishStop(attempt *stopAttempt) {
	complete := func(reports map[string]core.ObserverResult, err error, completed bool) {
		recorder.engine.closeIdleConnections()
		recorder.completeAttempt(attempt, reports, err, completed)
	}
	recorder.mu.Lock()
	cancel := recorder.sampleCancel
	done := recorder.sampleDone
	recorder.mu.Unlock()
	cancel()
	recorder.engine.closeIdleConnections()
	timer := time.NewTimer(recorder.stopTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		complete(nil, errors.New("monitor sampler did not stop before its timeout"), false)
		return
	}
	if err := recorder.reconcilePendingValidations(); err != nil {
		complete(nil, err, false)
		return
	}

	recorder.mu.Lock()
	if recorder.stopTime.IsZero() {
		recorder.stopTime = recorder.now().UTC()
	}
	stoppedAt := recorder.stopTime
	startedAt := recorder.startTime
	backgroundErr := recorder.backgroundErr
	artifacts := recorder.artifacts
	filesClosed := recorder.filesClosed
	recorder.mu.Unlock()
	if !filesClosed {
		for _, taskID := range recorder.taskIDs {
			if err := artifacts[taskID].closeResources(recorder.artifactOps); err != nil {
				complete(nil, err, false)
				return
			}
		}
		recorder.mu.Lock()
		recorder.filesClosed = true
		recorder.mu.Unlock()
	}

	status := core.StatusSucceeded
	errorText := ""
	if backgroundErr != nil {
		status = core.StatusFailed
		errorText = backgroundErr.Error()
	}
	duration := stoppedAt.Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	reports := make(map[string]core.ObserverResult, len(recorder.taskIDs))
	for _, taskID := range recorder.taskIDs {
		artifact := artifacts[taskID]
		index := Index{
			SchemaVersion:        indexSchemaVersion,
			RunID:                recorder.runID,
			TaskID:               taskID,
			Status:               status,
			Error:                errorText,
			StartedAt:            formatArtifactTime(startedAt),
			StoppedAt:            formatArtifactTime(stoppedAt),
			DurationNanoseconds:  duration.Nanoseconds(),
			IntervalMilliseconds: recorder.interval.Milliseconds(),
			SampleCount:          artifact.sequence,
			ResourcesFile:        "resources.jsonl",
			Components:           artifact.componentCoverage(),
		}
		if err := recorder.writeIndex(artifact.indexPath, index); err != nil {
			complete(nil, fmt.Errorf("write monitor index for task %s: %w", taskID, err), false)
			return
		}
		reports[taskID] = core.ObserverResult{
			Status:      status,
			Error:       errorText,
			Duration:    duration,
			SampleCount: int(artifact.sequence),
			LogPaths:    []string{artifact.resourcesPath, artifact.indexPath},
		}
	}
	complete(reports, nil, true)
	recorder.logger.Info("resource monitoring stopped", "run", recorder.runID, "status", status)
}

func (recorder *Recorder) completeAttempt(attempt *stopAttempt, reports map[string]core.ObserverResult, err error, completed bool) {
	recorder.mu.Lock()
	attempt.reports = cloneReports(reports)
	attempt.err = err
	attempt.finished = true
	if completed {
		recorder.completed = true
		recorder.reports = cloneReports(reports)
		recorder.sampleStates = nil
	}
	recorder.mu.Unlock()
	close(attempt.done)
}

func cloneReports(source map[string]core.ObserverResult) map[string]core.ObserverResult {
	if source == nil {
		return nil
	}
	clone := make(map[string]core.ObserverResult, len(source))
	for taskID, report := range source {
		report.LogPaths = append([]string(nil), report.LogPaths...)
		clone[taskID] = report
	}
	return clone
}

func validateIdentity(kind, value string) error {
	if value == "" || len(value) > maxIdentityLength {
		return fmt.Errorf("monitor %s ID length must be between 1 and %d", kind, maxIdentityLength)
	}
	for index, character := range value {
		allowed := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
		if !allowed || index == 0 && (character == '-' || character == '.') {
			return fmt.Errorf("monitor %s ID %q contains an unsafe character", kind, value)
		}
	}
	return nil
}
