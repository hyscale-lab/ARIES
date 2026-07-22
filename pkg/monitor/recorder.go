package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

const (
	defaultSampleInterval    = time.Second
	defaultRequestTimeout    = 5 * time.Second
	defaultStopTimeout       = 10 * time.Second
	defaultMaxSamplesPerTask = 172800
	defaultMaxFileBytes      = int64(128 << 20)
	maxIdentityLength        = 128
	maxMemoryBytes           = uint64(1 << 60)
	maxCPUPercent            = float64(1_000_000)
)

// Options are the explicit inputs to one run-scoped Recorder.
type Options struct {
	RunID             string
	TaskIDs           []string
	OutputDir         string
	Interval          time.Duration
	RequestTimeout    time.Duration
	StopTimeout       time.Duration
	MaxSamplesPerTask int
	MaxFileBytes      int64
	Source            ResourceSource
	Logger            *logrus.Logger
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
	logger            *logrus.Logger
	source            ResourceSource
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
	baselines     map[string]cpuBaseline
}

type stopAttempt struct {
	done     chan struct{}
	finished bool
	reports  map[string]core.ObserverResult
	err      error
}

type cpuBaseline struct {
	usage uint64
	time  time.Time
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
	if options.Source == nil {
		return nil, errors.New("monitor resource source is required")
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
		options.Logger = logrus.StandardLogger()
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
		source:            options.Source,
		artifactOps:       defaultArtifactOperations(),
		now:               time.Now,
		writeIndex:        writeIndexExact,
		baselines:         make(map[string]cpuBaseline),
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
		_ = recorder.source.Close()
		recorder.finishFailedStart()
		return err
	}
	startedAt := recorder.now().UTC()
	recorder.mu.Lock()
	recorder.artifacts = artifacts
	recorder.startTime = startedAt
	recorder.mu.Unlock()

	startFailure := func(cause error) error {
		_ = recorder.source.Close()
		rollbackErr := removePartialArtifacts(artifacts, monitorRoot, rootCreated, recorder.artifactOps)
		recorder.mu.Lock()
		recorder.artifacts = nil
		recorder.startTime = time.Time{}
		recorder.baselines = make(map[string]cpuBaseline)
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
	recorder.logger.WithContext(base).WithFields(logrus.Fields{"run": recorder.runID, "tasks": len(recorder.taskIDs)}).Info("resource monitoring started")
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
	readings, err := recorder.source.Sample(requestCtx)
	cancel()
	if err != nil {
		return err
	}
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].TaskID != readings[j].TaskID {
			return readings[i].TaskID < readings[j].TaskID
		}
		if readings[i].Component != readings[j].Component {
			return readings[i].Component < readings[j].Component
		}
		return readings[i].RuntimeID < readings[j].RuntimeID
	})
	present := make(map[string]struct{}, len(readings))
	for _, reading := range readings {
		if reading.ObservedAt.IsZero() {
			reading.ObservedAt = sampleTime
		}
		if err := recorder.validateReading(reading); err != nil {
			return err
		}
		key := reading.TaskID + "\x00" + reading.Component + "\x00" + reading.RuntimeID
		present[key] = struct{}{}
		cpuPercent, err := recorder.cpuPercent(key, reading)
		if err != nil {
			return err
		}
		artifact := recorder.artifacts[reading.TaskID]
		if artifact == nil {
			return fmt.Errorf("sampled unexpected task %q", reading.TaskID)
		}
		sample := ResourceSample{
			Second: second, Time: formatArtifactTime(reading.ObservedAt), TaskID: reading.TaskID,
			Component: reading.Component, RuntimeID: reading.RuntimeID, RuntimeName: reading.RuntimeName,
			CPUUsageNanoseconds: reading.CPUUsageNanoseconds, CPUPercent: cpuPercent,
			MemoryUsageBytes: reading.MemoryUsageBytes, MemoryLimitBytes: reading.MemoryLimitBytes,
		}
		if err := artifact.appendSample(sample, recorder.maxSamplesPerTask, recorder.maxFileBytes); err != nil {
			return fmt.Errorf("record monitor sample for task %s: %w", reading.TaskID, err)
		}
	}
	recorder.dropMissingBaselines(present)
	return nil
}

func (recorder *Recorder) validateReading(reading core.ResourceReading) error {
	if _, ok := recorder.taskSet[reading.TaskID]; !ok {
		return fmt.Errorf("sampled unexpected task %q", reading.TaskID)
	}
	if err := validateIdentity("component", reading.Component); err != nil {
		return err
	}
	if err := validateIdentity("runtime", reading.RuntimeID); err != nil {
		return err
	}
	if strings.TrimSpace(reading.RuntimeName) == "" || strings.ContainsAny(reading.RuntimeName, "\x00\r\n") {
		return errors.New("monitor runtime name is invalid")
	}
	if reading.MemoryUsageBytes > maxMemoryBytes || reading.MemoryLimitBytes > maxMemoryBytes {
		return errors.New("monitor memory measurement exceeds the bound")
	}
	return nil
}

func (recorder *Recorder) cpuPercent(key string, reading core.ResourceReading) (float64, error) {
	previous, ok := recorder.baselines[key]
	if !ok {
		recorder.baselines[key] = cpuBaseline{usage: reading.CPUUsageNanoseconds, time: reading.ObservedAt}
		return 0, nil
	}
	if !reading.ObservedAt.After(previous.time) {
		return 0, fmt.Errorf("monitor runtime %s observation time did not advance", reading.RuntimeID)
	}
	if reading.CPUUsageNanoseconds < previous.usage {
		return 0, fmt.Errorf("monitor runtime %s CPU counter decreased", reading.RuntimeID)
	}
	deltaCPU := reading.CPUUsageNanoseconds - previous.usage
	deltaWall := reading.ObservedAt.Sub(previous.time)
	percent := float64(deltaCPU) / float64(deltaWall) * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > maxCPUPercent {
		return 0, fmt.Errorf("monitor runtime %s CPU percentage %v is invalid", reading.RuntimeID, percent)
	}
	recorder.baselines[key] = cpuBaseline{usage: reading.CPUUsageNanoseconds, time: reading.ObservedAt}
	return percent, nil
}

func (recorder *Recorder) dropMissingBaselines(present map[string]struct{}) {
	for key := range recorder.baselines {
		if _, ok := present[key]; !ok {
			delete(recorder.baselines, key)
		}
	}
}

// Stop stops sampling independently of the caller's cancellation, finalizes
// private artifacts, and returns a cached exact report after success.
func (recorder *Recorder) Stop(ctx context.Context) (map[string]core.ObserverResult, error) {
	recorder.mu.Lock()
	if !recorder.started {
		starting := recorder.starting
		recorder.mu.Unlock()
		if !starting {
			_ = recorder.source.Close()
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
		if closeErr := recorder.source.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close monitor resource source: %w", closeErr))
			completed = false
		}
		recorder.completeAttempt(attempt, reports, err, completed)
	}
	recorder.mu.Lock()
	cancel := recorder.sampleCancel
	done := recorder.sampleDone
	recorder.mu.Unlock()
	cancel()
	timer := time.NewTimer(recorder.stopTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		complete(nil, errors.New("monitor sampler did not stop before its timeout"), false)
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
	recorder.logger.WithFields(logrus.Fields{"run": recorder.runID, "status": status}).Info("resource monitoring stopped")
}

func (recorder *Recorder) completeAttempt(attempt *stopAttempt, reports map[string]core.ObserverResult, err error, completed bool) {
	recorder.mu.Lock()
	attempt.reports = cloneReports(reports)
	attempt.err = err
	attempt.finished = true
	if completed {
		recorder.completed = true
		recorder.reports = cloneReports(reports)
		recorder.baselines = nil
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

func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
