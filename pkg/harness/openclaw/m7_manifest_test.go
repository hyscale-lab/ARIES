package openclaw

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
)

const (
	m7ManifestName          = "manifest.json"
	m7SchemaVersion         = "aries.m7.monitored.v1"
	m7CanonicalM6SHA256     = "84c17fe2ff6717907528221d461b5992f227dd6970f76e21cf908fe42cdc2143"
	m7RequireCanonicalM6Env = "ARIES_REQUIRE_CANONICAL_M6"
	m7MaxManifestBytes      = int64(1 << 20)
	m7MaxResourceBytes      = int64(16 << 20)
	m7MaxResourceSamples    = 100_000
	m7MaxArtifacts          = 64
	m7MaxArtifactBytes      = int64(16 << 20)
	m7MaxArtifactTotalBytes = int64(64 << 20)
	m7ResourceIntervalMS    = int64(1000)
	m7RoleMonitorResources  = "monitor_resources"
	m7RoleMonitorIndex      = "monitor_index"
)

type m7Manifest struct {
	SchemaVersion  string                   `json:"schema_version"`
	RunID          string                   `json:"run_id"`
	TaskID         string                   `json:"task_id"`
	Pins           m6Pins                   `json:"pins"`
	Outcomes       m7PortableTaskResult     `json:"outcomes"`
	RunResult      m6ArtifactReference      `json:"run_result"`
	Observer       m7ObserverEvidence       `json:"observer"`
	Verifier       m6VerifierEvidence       `json:"verifier"`
	LiveValidation m7LiveValidationEvidence `json:"live_validation"`
	M6Preservation m7M6PreservationEvidence `json:"m6_preservation"`
	Artifacts      []m7ArtifactEntry        `json:"artifacts"`
	Inventory      m6ResourceInventory      `json:"resource_inventory"`
}

type m7PortableRunResult struct {
	Name     string                 `json:"name"`
	RunID    string                 `json:"run_id"`
	Tasks    []m7PortableTaskResult `json:"tasks"`
	Summary  m6PortableRunSummary   `json:"summary"`
	Duration time.Duration          `json:"duration"`
}

type m7PortableTaskResult struct {
	TaskID     string                     `json:"task_id"`
	Harness    m6PortableHarnessResult    `json:"harness"`
	Isolation  m6PortableIsolationResult  `json:"isolation"`
	Evaluation m6PortableEvaluationResult `json:"evaluation"`
	Observer   m7PortableObserverResult   `json:"observer"`
	Cleanup    m6PortableStatusResult     `json:"cleanup"`
	Duration   time.Duration              `json:"duration"`
}

type m7PortableObserverResult struct {
	Status      string                `json:"status"`
	Duration    time.Duration         `json:"duration"`
	SampleCount int                   `json:"sample_count"`
	Artifacts   []m6ArtifactReference `json:"artifacts"`
	Error       string                `json:"error,omitempty"`
}

type m7ObserverEvidence struct {
	Status               string              `json:"status"`
	IntervalMilliseconds int64               `json:"interval_milliseconds"`
	SampleCount          int                 `json:"sample_count"`
	SharedSecond         uint64              `json:"shared_second"`
	Resources            m6ArtifactReference `json:"resources"`
	Index                m6ArtifactReference `json:"index"`
}

type m7LiveValidationEvidence struct {
	Status   string              `json:"status"`
	Attempts int                 `json:"attempts"`
	Reason   string              `json:"reason"`
	Artifact m6ArtifactReference `json:"artifact"`
}

type m7M6PreservationEvidence struct {
	ManifestCount   int                 `json:"manifest_count"`
	CanonicalExists bool                `json:"canonical_exists"`
	CanonicalSHA256 string              `json:"canonical_sha256,omitempty"`
	Artifact        m6ArtifactReference `json:"artifact"`
}

type m7M6PreservationArtifact struct {
	Before []m7M6ManifestSnapshot `json:"before"`
	After  []m7M6ManifestSnapshot `json:"after"`
}

type m7M6ManifestSnapshot struct {
	Path             string `json:"path"`
	Mode             uint32 `json:"mode"`
	Size             int64  `json:"size"`
	Inode            uint64 `json:"inode"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
	SHA256           string `json:"sha256"`
}

type m7ArtifactEntry struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type m7ArtifactLimits struct {
	Count          int
	PerArtifact    int64
	AggregateBytes int64
}

type m7ReadStatCloser interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type m7FileOperations struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (m7ReadStatCloser, error)
}

var m7ReleaseArtifactLimits = m7ArtifactLimits{
	Count: m7MaxArtifacts, PerArtifact: m7MaxArtifactBytes, AggregateBytes: m7MaxArtifactTotalBytes,
}

func defaultM7FileOperations() m7FileOperations {
	return m7FileOperations{
		lstat: os.Lstat,
		open: func(path string) (m7ReadStatCloser, error) {
			return os.Open(path)
		},
	}
}

func newM7PortableRunResult(root string, result core.RunResult) (m7PortableRunResult, error) {
	tasks := make([]m7PortableTaskResult, 0, len(result.Tasks))
	for _, task := range result.Tasks {
		harnessArtifacts, err := newM6ArtifactReferences(root, task.Harness.LogPaths, m6HarnessArtifactRole)
		if err != nil {
			return m7PortableRunResult{}, fmt.Errorf("normalize M7 harness artifacts: %w", err)
		}
		evaluationArtifacts, err := newM6ArtifactReferences(root, task.Evaluation.LogPaths, m6EvaluationArtifactRole)
		if err != nil {
			return m7PortableRunResult{}, fmt.Errorf("normalize M7 evaluation artifacts: %w", err)
		}
		observerArtifacts, err := newM7ObserverReferences(root, task.Observer.LogPaths)
		if err != nil {
			return m7PortableRunResult{}, err
		}
		tasks = append(tasks, m7PortableTaskResult{
			TaskID:     task.TaskID,
			Harness:    m6PortableHarnessResult{Status: task.Harness.Status, FinalResponse: task.Harness.FinalResponse, Duration: task.Harness.Duration, Artifacts: harnessArtifacts, Error: task.Harness.Error},
			Isolation:  m6PortableIsolationResult{Status: task.Isolation.Status, HarnessStopped: task.Isolation.HarnessStopped, BridgeRevoked: task.Isolation.BridgeRevoked, Error: task.Isolation.Error},
			Evaluation: m6PortableEvaluationResult{Status: task.Evaluation.Status, Score: task.Evaluation.Score, Reward: task.Evaluation.Reward, VerifierStatus: task.Evaluation.VerifierStatus, Duration: task.Evaluation.Duration, Artifacts: evaluationArtifacts, Error: task.Evaluation.Error},
			Observer:   m7PortableObserverResult{Status: task.Observer.Status, Duration: task.Observer.Duration, SampleCount: task.Observer.SampleCount, Artifacts: observerArtifacts, Error: task.Observer.Error},
			Cleanup:    m6PortableStatusResult{Status: task.Cleanup.Status, Error: task.Cleanup.Error}, Duration: task.Duration,
		})
	}
	return m7PortableRunResult{Name: result.Name, RunID: result.RunID, Tasks: tasks, Summary: m6PortableRunSummary{
		Tasks: result.Summary.Tasks, HarnessSucceeded: result.Summary.HarnessSucceeded, HarnessFailed: result.Summary.HarnessFailed,
		EvaluationsRun: result.Summary.EvaluationsRun, EvaluationsSucceeded: result.Summary.EvaluationsSucceeded,
		EvaluationsFailed: result.Summary.EvaluationsFailed, EvaluationsBlocked: result.Summary.EvaluationsBlocked,
		CleanupFailed: result.Summary.CleanupFailed,
	}, Duration: result.Duration}, nil
}

func newM7ObserverReferences(root string, paths []string) ([]m6ArtifactReference, error) {
	if len(paths) != 2 {
		return nil, fmt.Errorf("M7 observer artifact paths = %#v, want resources and index", paths)
	}
	references := make([]m6ArtifactReference, 0, 2)
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("M7 observer path %q is not absolute", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		relative = filepath.ToSlash(relative)
		if _, err := m6ArtifactPath(root, relative); err != nil {
			return nil, fmt.Errorf("M7 observer path %q is invalid: %w", relative, err)
		}
		if !strings.HasPrefix(relative, "monitor/fix-git/") {
			return nil, fmt.Errorf("M7 observer path %q is outside the task monitor directory", relative)
		}
		switch filepath.Base(relative) {
		case "resources.jsonl":
			references = append(references, m6ArtifactReference{Role: m7RoleMonitorResources, Path: relative})
		case "index.json":
			references = append(references, m6ArtifactReference{Role: m7RoleMonitorIndex, Path: relative})
		default:
			return nil, fmt.Errorf("unknown M7 observer artifact %q", relative)
		}
	}
	slices.SortFunc(references, func(left, right m6ArtifactReference) int { return strings.Compare(left.Role, right.Role) })
	if references[0].Role == references[1].Role {
		return nil, errors.New("M7 observer artifact roles are duplicated")
	}
	return references, nil
}

func newM7EvidenceDir(repositoryRoot string) (string, error) {
	root := filepath.Join(repositoryRoot, ".cache", "integration", "m7")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create M7 integration root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("make M7 integration root private: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("M7 integration root is not a private real directory: %v", err)
	}
	directory, err := os.MkdirTemp(root, "fix-git-")
	if err != nil {
		return "", fmt.Errorf("create unique M7 evidence directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", err
	}
	return directory, nil
}

func collectM7ArtifactEntries(root string) ([]m7ArtifactEntry, error) {
	return collectM7ArtifactEntriesWithLimits(root, m7ReleaseArtifactLimits)
}

func collectM7ArtifactEntriesWithLimits(root string, limits m7ArtifactLimits) ([]m7ArtifactEntry, error) {
	if limits.Count <= 0 || limits.PerArtifact <= 0 || limits.AggregateBytes <= 0 {
		return nil, errors.New("M7 artifact bounds must be positive")
	}
	var entries []m7ArtifactEntry
	var aggregateBytes int64
	type identifiedFile struct {
		path string
		info os.FileInfo
	}
	var identified []identifiedFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("M7 artifact tree contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == m7ManifestName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if len(entries) >= limits.Count {
			return fmt.Errorf("M7 artifact count exceeds %d", limits.Count)
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limits.PerArtifact {
			return fmt.Errorf("M7 artifact %q exceeds the per-artifact bound %d", relative, limits.PerArtifact)
		}
		if info.Size() > limits.AggregateBytes-aggregateBytes {
			return fmt.Errorf("M7 artifact aggregate exceeds %d bytes", limits.AggregateBytes)
		}
		for _, prior := range identified {
			if os.SameFile(prior.info, info) {
				return fmt.Errorf("M7 artifacts %q and %q alias the same file", prior.path, relative)
			}
		}
		identified = append(identified, identifiedFile{path: relative, info: info})
		role, err := m7ArtifactRole(relative)
		if err != nil {
			return err
		}
		hashed, err := hashM7Artifact(root, relative, limits.PerArtifact)
		if err != nil {
			return err
		}
		entries = append(entries, m7ArtifactEntry{Role: role, Path: hashed.Path, SHA256: hashed.SHA256, Size: hashed.Size})
		aggregateBytes += hashed.Size
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(left, right m7ArtifactEntry) int { return strings.Compare(left.Path, right.Path) })
	if entries == nil {
		entries = []m7ArtifactEntry{}
	}
	return entries, nil
}

func hashM7Artifact(root, relative string, maximum int64) (m6ArtifactEntry, error) {
	return hashM7ArtifactWithOperations(root, relative, maximum, defaultM7FileOperations())
}

func hashM7ArtifactWithOperations(root, relative string, maximum int64, operations m7FileOperations) (m6ArtifactEntry, error) {
	if maximum <= 0 || operations.lstat == nil || operations.open == nil {
		return m6ArtifactEntry{}, errors.New("M7 artifact hashing requires positive bounds and file operations")
	}
	path, err := m6ArtifactPath(root, relative)
	if err != nil {
		return m6ArtifactEntry{}, err
	}
	if err := rejectM6SymlinkPath(root, relative); err != nil {
		return m6ArtifactEntry{}, err
	}
	before, err := operations.lstat(path)
	if err != nil {
		return m6ArtifactEntry{}, fmt.Errorf("inspect M7 artifact %q: %w", relative, err)
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maximum {
		return m6ArtifactEntry{}, fmt.Errorf("M7 artifact %q exceeds the per-artifact bound %d", relative, maximum)
	}
	file, err := operations.open(path)
	if err != nil {
		return m6ArtifactEntry{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	pathAfter, pathStatErr := operations.lstat(path)
	closeErr := file.Close()
	if copyErr != nil || statErr != nil || pathStatErr != nil || closeErr != nil {
		return m6ArtifactEntry{}, errors.Join(copyErr, statErr, pathStatErr, closeErr)
	}
	if size > maximum {
		return m6ArtifactEntry{}, fmt.Errorf("M7 artifact %q grew beyond %d bytes while hashing", relative, maximum)
	}
	if !sameM7FileSnapshot(before, after) || !sameM7FileSnapshot(before, pathAfter) || size != before.Size() {
		return m6ArtifactEntry{}, fmt.Errorf("M7 artifact %q changed while hashing", relative)
	}
	return m6ArtifactEntry{Path: relative, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}, nil
}

func sameM7FileSnapshot(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() &&
		before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && before.Mode().IsRegular() && after.Mode().IsRegular()
}

func m7ArtifactRole(relative string) (string, error) {
	switch relative {
	case "fake-model-evidence/ready":
		return "fake_model_ready", nil
	case "fake-model-evidence/status.json":
		return "fake_model_status", nil
	case "fake-model-evidence/transcript.jsonl":
		return "fake_model_transcript", nil
	case "fix-git/evaluation/ctrf.json":
		return m6RoleVerifierCTRF, nil
	case "fix-git/evaluation/reward.txt":
		return m6RoleVerifierReward, nil
	case "fix-git/evaluation/stderr.log":
		return m6RoleVerifierStderr, nil
	case "fix-git/evaluation/stdout.log":
		return m6RoleVerifierStdout, nil
	case "m7/cleanup-inventory.json":
		return "cleanup_inventory", nil
	case "m7/git-filesystem-delta.json":
		return "git_filesystem_delta", nil
	case "m7/isolation-trace.json":
		return "isolation_trace", nil
	case "m7/live-validation.json":
		return "live_validation", nil
	case "m7/m6-preservation.json":
		return "m6_preservation", nil
	case "m7/model-protocol.jsonl":
		return "model_protocol", nil
	case "m7/run-result.json":
		return m6RoleRunResult, nil
	case "monitor/fix-git/resources.jsonl":
		return m7RoleMonitorResources, nil
	case "monitor/fix-git/index.json":
		return m7RoleMonitorIndex, nil
	}
	if strings.HasPrefix(relative, "harnesses/") {
		switch {
		case filepath.Base(relative) == "agent.json":
			return m6RoleOpenClawAgentResult, nil
		case filepath.Base(relative) == "agent.stderr":
			return m6RoleOpenClawAgentStderr, nil
		case filepath.Base(relative) == "gateway.log":
			return m6RoleOpenClawGatewayLog, nil
		case filepath.Base(relative) == "telemetry.index.json":
			return m6RoleOpenClawTelemetryIndex, nil
		case strings.HasSuffix(relative, ".trajectory.jsonl"):
			return "openclaw_trajectory", nil
		case strings.HasSuffix(relative, ".trajectory-path.json"):
			return "openclaw_trajectory_path", nil
		case filepath.Base(relative) == "sessions.json":
			return "openclaw_sessions", nil
		case strings.HasSuffix(relative, ".jsonl"):
			return "openclaw_telemetry", nil
		}
	}
	if strings.HasPrefix(relative, "sandboxes/") {
		switch filepath.Base(relative) {
		case "container.stdout.log":
			return "sandbox_stdout", nil
		case "container.stderr.log":
			return "sandbox_stderr", nil
		case "aries-exec-helper":
			return "sandbox_exec_helper", nil
		}
	}
	return "", fmt.Errorf("M7 artifact %q has no concrete role", relative)
}

func writeM7Manifest(root string, manifest m7Manifest) error {
	if err := validateM7Manifest(root, manifest); err != nil {
		return err
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(root, m7ManifestName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create M7 manifest exclusively: %w", err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(content, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return err
	}
	readBack, err := readM7Manifest(root)
	if err != nil {
		return err
	}
	if err := validateM7Manifest(root, readBack); err != nil {
		return err
	}
	complete = true
	return nil
}

func readM7Manifest(root string) (m7Manifest, error) {
	var manifest m7Manifest
	err := consumeStableM7File(root, m7ManifestName, m7MaxManifestBytes, 0o400, func(reader io.Reader) error {
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			return fmt.Errorf("decode M7 manifest: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return errors.New("M7 manifest has trailing JSON content")
		}
		return nil
	})
	if err != nil {
		return m7Manifest{}, err
	}
	return manifest, nil
}

func validateM7Manifest(root string, manifest m7Manifest) error {
	if manifest.SchemaVersion != m7SchemaVersion || manifest.RunID == "" || manifest.TaskID != "fix-git" {
		return errors.New("M7 manifest schema or identity is invalid")
	}
	if manifest.Pins != (m6Pins{TerminalBenchRevision: terminalbench.Revision, OpenClawImage: PinnedImage, TaskImage: m6FixGitTaskImage, Model: "aries-deterministic"}) {
		return errors.New("M7 manifest pins are not exact")
	}
	outcomes := manifest.Outcomes
	if outcomes.TaskID != manifest.TaskID || outcomes.Harness.Status != core.StatusSucceeded || outcomes.Harness.Error != "" || outcomes.Isolation.Status != core.StatusConfirmed || !outcomes.Isolation.HarnessStopped || !outcomes.Isolation.BridgeRevoked || outcomes.Isolation.Error != "" || outcomes.Evaluation.Status != core.StatusSucceeded || outcomes.Evaluation.VerifierStatus != core.StatusSucceeded || outcomes.Evaluation.Score != 1 || outcomes.Evaluation.Reward != 1 || outcomes.Evaluation.Error != "" || outcomes.Observer.Status != core.StatusSucceeded || outcomes.Observer.Error != "" || outcomes.Observer.SampleCount <= 0 || outcomes.Cleanup.Status != core.StatusSucceeded || outcomes.Cleanup.Error != "" {
		return errors.New("M7 manifest outcomes are not the successful separated monitored Runner result")
	}
	if manifest.Verifier.RewardBytes != "1\n" || !slices.Equal(manifest.Verifier.Cases, m6ExpectedVerifierCases) || !slices.Equal(manifest.Verifier.Sources, m6ExpectedVerifierSources) {
		return errors.New("M7 verifier evidence is not exact")
	}
	if len(manifest.Artifacts) == 0 || len(manifest.Artifacts) > m7MaxArtifacts {
		return fmt.Errorf("M7 manifest artifact count must be between 1 and %d", m7MaxArtifacts)
	}
	actualArtifacts, err := collectM7ArtifactEntries(root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actualArtifacts, manifest.Artifacts) {
		return errors.New("M7 artifact table is incomplete, stale, duplicated, unsorted, or null")
	}
	artifacts := make(map[string]m7ArtifactEntry, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.Path == m7ManifestName {
			return errors.New("M7 manifest cannot reference itself")
		}
		if _, exists := artifacts[artifact.Path]; exists {
			return fmt.Errorf("duplicate M7 artifact %q", artifact.Path)
		}
		artifacts[artifact.Path] = artifact
	}
	if err := validateM7RequiredArtifacts(artifacts); err != nil {
		return err
	}
	if err := validateM7OutcomeReferences(root, outcomes, artifacts); err != nil {
		return err
	}
	if manifest.RunResult != (m6ArtifactReference{Role: m6RoleRunResult, Path: "m7/run-result.json"}) {
		return errors.New("M7 run-result reference is not exact")
	}
	portable, err := readM7JSONArtifact[m7PortableRunResult](root, manifest.RunResult.Path, m6MaxRunResultBytes)
	if err != nil {
		return err
	}
	if portable.RunID != manifest.RunID || len(portable.Tasks) != 1 || !reflect.DeepEqual(portable.Tasks[0], outcomes) || portable.Summary != successfulM6RunSummary() {
		return errors.New("M7 run-result artifact does not match manifest outcomes")
	}
	if err := validateM7Observer(root, manifest, artifacts); err != nil {
		return err
	}
	if err := validateM7LiveValidation(root, manifest.LiveValidation, artifacts); err != nil {
		return err
	}
	requireCanonicalM6, err := requireCanonicalM6FromEnv()
	if err != nil {
		return err
	}
	if err := validateM7Preservation(root, manifest.M6Preservation, artifacts, requireCanonicalM6); err != nil {
		return err
	}
	if !emptyM7Inventory(manifest.Inventory) {
		return errors.New("M7 final ARIES resource inventory is not empty")
	}
	return nil
}

func emptyM7Inventory(inventory m6ResourceInventory) bool {
	return inventory.Containers != nil && inventory.Volumes != nil && inventory.Networks != nil &&
		len(inventory.Containers) == 0 && len(inventory.Volumes) == 0 && len(inventory.Networks) == 0
}

func validateM7RequiredArtifacts(artifacts map[string]m7ArtifactEntry) error {
	required := []string{
		"fake-model-evidence/ready", "fake-model-evidence/status.json", "fake-model-evidence/transcript.jsonl",
		"fix-git/evaluation/ctrf.json", "fix-git/evaluation/reward.txt", "fix-git/evaluation/stderr.log", "fix-git/evaluation/stdout.log",
		"m7/cleanup-inventory.json", "m7/git-filesystem-delta.json", "m7/isolation-trace.json", "m7/live-validation.json",
		"m7/m6-preservation.json", "m7/model-protocol.jsonl", "m7/run-result.json",
		"monitor/fix-git/index.json", "monitor/fix-git/resources.jsonl",
	}
	for _, path := range required {
		if _, ok := artifacts[path]; !ok {
			return fmt.Errorf("M7 manifest is missing required artifact %q", path)
		}
	}
	for _, base := range []string{"agent.json", "agent.stderr", "gateway.log", "telemetry.index.json", "container.stdout.log", "container.stderr.log", "aries-exec-helper"} {
		found := false
		for path := range artifacts {
			if filepath.Base(path) == base {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("M7 manifest is missing artifact %q", base)
		}
	}
	foundTrajectory := false
	for path := range artifacts {
		if strings.HasSuffix(path, ".trajectory.jsonl") {
			foundTrajectory = true
		}
	}
	if !foundTrajectory {
		return errors.New("M7 manifest is missing OpenClaw trajectory telemetry")
	}
	return nil
}

func validateM7OutcomeReferences(root string, outcomes m7PortableTaskResult, artifacts map[string]m7ArtifactEntry) error {
	m6Outcomes := m6PortableTaskResult{TaskID: outcomes.TaskID, Harness: outcomes.Harness, Isolation: outcomes.Isolation, Evaluation: outcomes.Evaluation, Observer: m6PortableStatusResult{Status: outcomes.Observer.Status, Error: outcomes.Observer.Error}, Cleanup: outcomes.Cleanup, Duration: outcomes.Duration}
	m6Artifacts := make(map[string]m6ArtifactEntry, len(artifacts))
	for path, artifact := range artifacts {
		m6Artifacts[path] = m6ArtifactEntry{Path: path, SHA256: artifact.SHA256, Size: artifact.Size}
	}
	if err := validateM6OutcomeArtifactReferences(root, m6Outcomes, m6Artifacts); err != nil {
		return err
	}
	expected := []m6ArtifactReference{{Role: m7RoleMonitorIndex, Path: "monitor/fix-git/index.json"}, {Role: m7RoleMonitorResources, Path: "monitor/fix-git/resources.jsonl"}}
	if !slices.Equal(outcomes.Observer.Artifacts, expected) {
		return errors.New("M7 observer outcome artifact references are not exact")
	}
	for _, reference := range expected {
		entry, ok := artifacts[reference.Path]
		if !ok || entry.Role != reference.Role {
			return fmt.Errorf("M7 observer reference %q is not linked to its typed artifact", reference.Path)
		}
	}
	return nil
}

func validateM7Observer(root string, manifest m7Manifest, artifacts map[string]m7ArtifactEntry) error {
	evidence := manifest.Observer
	if evidence.Status != core.StatusSucceeded || evidence.IntervalMilliseconds != m7ResourceIntervalMS || evidence.SampleCount <= 0 || evidence.SampleCount != manifest.Outcomes.Observer.SampleCount {
		return errors.New("M7 observer summary is invalid")
	}
	if evidence.Resources != (m6ArtifactReference{Role: m7RoleMonitorResources, Path: "monitor/fix-git/resources.jsonl"}) || evidence.Index != (m6ArtifactReference{Role: m7RoleMonitorIndex, Path: "monitor/fix-git/index.json"}) {
		return errors.New("M7 observer evidence references are not exact")
	}
	if artifacts[evidence.Resources.Path].Role != evidence.Resources.Role || artifacts[evidence.Index.Path].Role != evidence.Index.Role {
		return errors.New("M7 observer evidence is not linked to typed artifacts")
	}
	index, err := readM7JSONArtifact[monitor.Index](root, evidence.Index.Path, 1<<20)
	if err != nil {
		return fmt.Errorf("read M7 monitor index: %w", err)
	}
	if index.SchemaVersion != 1 || index.RunID != manifest.RunID || index.TaskID != manifest.TaskID || index.Status != core.StatusSucceeded || index.Error != "" || index.IntervalMilliseconds != m7ResourceIntervalMS || index.SampleCount != uint64(evidence.SampleCount) || index.ResourcesFile != "resources.jsonl" {
		return errors.New("M7 monitor index is inconsistent")
	}
	startedAt, startErr := time.Parse(time.RFC3339Nano, index.StartedAt)
	stoppedAt, stopErr := time.Parse(time.RFC3339Nano, index.StoppedAt)
	if startErr != nil || stopErr != nil || stoppedAt.Before(startedAt) || index.DurationNanoseconds != stoppedAt.Sub(startedAt).Nanoseconds() || time.Duration(index.DurationNanoseconds) != manifest.Outcomes.Observer.Duration {
		return errors.New("M7 monitor index timing is inconsistent")
	}
	samples, err := readM7ResourceSamples(root, evidence.Resources.Path)
	if err != nil {
		return err
	}
	if len(samples) != evidence.SampleCount {
		return errors.New("M7 monitor resource sample count is inconsistent")
	}
	seconds := map[string]map[uint64]struct{}{"task-container": {}, "openclaw-harness": {}}
	counts := map[string]uint64{}
	for sequence, sample := range samples {
		if _, err := time.Parse(time.RFC3339Nano, sample.Time); err != nil {
			return fmt.Errorf("M7 monitor sample %d has invalid time: %w", sequence, err)
		}
		if sample.Sequence != uint64(sequence) || sample.TaskID != manifest.TaskID || sample.ContainerID == "" || sample.ContainerName == "" || !isM7FiniteNonnegative(sample.CPUPercent) || sample.CPUPercent > 1_000_000 || sample.MemoryBytes > sample.MemoryLimitBytes || sample.MemoryLimitBytes > 1<<60 {
			return fmt.Errorf("M7 monitor sample %d is invalid", sequence)
		}
		if _, ok := seconds[sample.Component]; !ok {
			return fmt.Errorf("M7 monitor sample has unexpected component %q", sample.Component)
		}
		seconds[sample.Component][sample.Second] = struct{}{}
		counts[sample.Component]++
	}
	if counts["task-container"] == 0 || counts["openclaw-harness"] == 0 {
		return errors.New("M7 monitor did not sample both sandbox and harness")
	}
	shared := false
	for second := range seconds["task-container"] {
		if _, ok := seconds["openclaw-harness"][second]; ok {
			if second == evidence.SharedSecond {
				shared = true
			}
		}
	}
	if !shared {
		return errors.New("M7 monitor has no declared shared sandbox/harness sample second")
	}
	coverage := map[string]uint64{}
	for _, component := range index.Components {
		coverage[component.Component] += component.SampleCount
	}
	if coverage["task-container"] != counts["task-container"] || coverage["openclaw-harness"] != counts["openclaw-harness"] || len(coverage) != 2 {
		return errors.New("M7 monitor index coverage does not match resource samples")
	}
	return nil
}

func validateM7LiveValidation(root string, evidence m7LiveValidationEvidence, artifacts map[string]m7ArtifactEntry) error {
	expectedRef := m6ArtifactReference{Role: "live_validation", Path: "m7/live-validation.json"}
	if evidence.Status != "not_requested" || evidence.Attempts != 0 || evidence.Reason != "deterministic_fake_model" || evidence.Artifact != expectedRef || artifacts[expectedRef.Path].Role != expectedRef.Role {
		return errors.New("M7 deterministic live-validation evidence is invalid")
	}
	artifact, err := readM7JSONArtifact[m7LiveValidationArtifact](root, expectedRef.Path, 4096)
	if err != nil {
		return err
	}
	if artifact.Status != evidence.Status || artifact.Attempts != 0 || artifact.Reason != evidence.Reason {
		return errors.New("M7 live-validation artifact does not match manifest")
	}
	return nil
}

type m7LiveValidationArtifact struct {
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Reason   string `json:"reason"`
}

func requireCanonicalM6FromEnv() (bool, error) {
	value, exists := os.LookupEnv(m7RequireCanonicalM6Env)
	if !exists || value == "" {
		return false, nil
	}
	if value == "1" {
		return true, nil
	}
	return false, fmt.Errorf("%s must be unset, empty, or exactly 1", m7RequireCanonicalM6Env)
}

func validateM7Preservation(root string, evidence m7M6PreservationEvidence, artifacts map[string]m7ArtifactEntry, requireCanonical bool) error {
	expectedRef := m6ArtifactReference{Role: "m6_preservation", Path: "m7/m6-preservation.json"}
	if evidence.ManifestCount < 0 || evidence.Artifact != expectedRef || artifacts[expectedRef.Path].Role != expectedRef.Role {
		return errors.New("M7 M6-preservation reference is invalid")
	}
	artifact, err := readM7JSONArtifact[m7M6PreservationArtifact](root, expectedRef.Path, 1<<20)
	if err != nil {
		return err
	}
	if err := requireSameM6Snapshots(artifact.Before, artifact.After); err != nil {
		return err
	}
	if err := validateM7M6Snapshots(artifact.Before); err != nil {
		return err
	}
	if evidence.ManifestCount != len(artifact.Before) {
		return errors.New("M7 M6-preservation count is inconsistent")
	}
	canonicalExists := false
	for _, snapshot := range artifact.Before {
		if snapshot.SHA256 == m7CanonicalM6SHA256 {
			canonicalExists = true
		}
	}
	if evidence.CanonicalExists != canonicalExists {
		return errors.New("M7 canonical M6-preservation status is inconsistent")
	}
	if canonicalExists && evidence.CanonicalSHA256 != m7CanonicalM6SHA256 {
		return errors.New("M7 canonical M6-preservation hash is inconsistent")
	}
	if !canonicalExists && evidence.CanonicalSHA256 != "" {
		return errors.New("M7 M6-preservation has a canonical hash without a canonical manifest")
	}
	if requireCanonical && !canonicalExists {
		return errors.New("M7 canonical M6 manifest is required but missing from preservation evidence")
	}
	return nil
}

func validateM7M6Snapshots(snapshots []m7M6ManifestSnapshot) error {
	previous := ""
	for _, snapshot := range snapshots {
		if snapshot.Path == "" || filepath.IsAbs(snapshot.Path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(snapshot.Path))) != snapshot.Path || !strings.HasPrefix(snapshot.Path, ".cache/integration/m6/") || filepath.Base(snapshot.Path) != m6ManifestName {
			return fmt.Errorf("M7 M6-preservation path %q is not canonical and repository-relative", snapshot.Path)
		}
		if previous != "" && strings.Compare(previous, snapshot.Path) >= 0 {
			return errors.New("M7 M6-preservation snapshots are not strictly sorted and unique")
		}
		previous = snapshot.Path
		digest, err := hex.DecodeString(snapshot.SHA256)
		if err != nil || len(digest) != sha256.Size || snapshot.Mode != 0o400 || snapshot.Size <= 0 || snapshot.Size > m7MaxManifestBytes || snapshot.Inode == 0 || snapshot.ModifiedUnixNano <= 0 {
			return fmt.Errorf("M7 M6-preservation metadata for %q is invalid", snapshot.Path)
		}
	}
	return nil
}

func readM7ResourceSamples(root, relative string) ([]monitor.ResourceSample, error) {
	samples := make([]monitor.ResourceSample, 0)
	err := consumeStableM7File(root, relative, m7MaxResourceBytes, 0o600, func(reader io.Reader) error {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			if len(samples) >= m7MaxResourceSamples {
				return errors.New("M7 monitor sample count exceeds bound")
			}
			decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
			decoder.DisallowUnknownFields()
			var sample monitor.ResourceSample
			if err := decoder.Decode(&sample); err != nil {
				return fmt.Errorf("decode M7 monitor sample: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return errors.New("M7 monitor sample has trailing JSON")
			}
			samples = append(samples, sample)
		}
		return scanner.Err()
	})
	if err != nil {
		return nil, err
	}
	return samples, nil
}

func readM7JSONArtifact[T any](root, relative string, maximum int64) (T, error) {
	var value T
	err := consumeStableM7File(root, relative, maximum, 0o600, func(reader io.Reader) error {
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return errors.New("M7 JSON artifact has trailing content")
		}
		return nil
	})
	if err != nil {
		return value, err
	}
	return value, nil
}

func consumeStableM7File(root, relative string, maximum int64, mode os.FileMode, consume func(io.Reader) error) error {
	return consumeStableM7FileWithOperations(root, relative, maximum, mode, consume, defaultM7FileOperations())
}

func consumeStableM7FileWithOperations(root, relative string, maximum int64, mode os.FileMode, consume func(io.Reader) error, operations m7FileOperations) error {
	if maximum <= 0 || consume == nil || operations.lstat == nil || operations.open == nil {
		return errors.New("M7 stable file read requires a positive bound, consumer, and file operations")
	}
	path, err := m6ArtifactPath(root, relative)
	if err != nil {
		return err
	}
	if err := rejectM6SymlinkPath(root, relative); err != nil {
		return err
	}
	before, err := operations.lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != mode || before.Size() < 0 || before.Size() > maximum {
		return fmt.Errorf("M7 file %q type/mode/size is invalid", relative)
	}
	file, err := operations.open(path)
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: file, N: maximum + 1}
	consumeErr := consume(limited)
	after, statErr := file.Stat()
	pathAfter, pathStatErr := operations.lstat(path)
	closeErr := file.Close()
	if consumeErr != nil || statErr != nil || pathStatErr != nil || closeErr != nil {
		return errors.Join(consumeErr, statErr, pathStatErr, closeErr)
	}
	readBytes := maximum + 1 - limited.N
	if readBytes > maximum || !sameM7FileSnapshot(before, after) || !sameM7FileSnapshot(before, pathAfter) || readBytes != before.Size() {
		return errors.New("M7 file changed or exceeded its bound while reading")
	}
	return nil
}

func snapshotM6Manifests(repositoryRoot string, requireCanonical bool) ([]m7M6ManifestSnapshot, bool, error) {
	return snapshotM6ManifestsWithValidator(repositoryRoot, func(root string) error {
		_, err := readM6Manifest(root)
		return err
	}, requireCanonical, m7CanonicalM6SHA256)
}

func snapshotM6ManifestsWithValidator(repositoryRoot string, validate func(string) error, requireCanonical bool, expectedCanonicalSHA256 string) ([]m7M6ManifestSnapshot, bool, error) {
	if validate == nil {
		return nil, false, errors.New("M6 manifest snapshot validator is required")
	}
	if digest, err := hex.DecodeString(expectedCanonicalSHA256); err != nil || len(digest) != sha256.Size {
		return nil, false, errors.New("expected canonical M6 SHA256 is invalid")
	}
	searchRoot := filepath.Join(repositoryRoot, ".cache", "integration", "m6")
	if _, err := os.Lstat(searchRoot); errors.Is(err, os.ErrNotExist) {
		if requireCanonical {
			return nil, false, errors.New("canonical M6 manifest is required but the M6 evidence root is absent")
		}
		return []m7M6ManifestSnapshot{}, false, nil
	} else if err != nil {
		return nil, false, err
	}
	var snapshots []m7M6ManifestSnapshot
	canonicalExists := false
	err := filepath.WalkDir(searchRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Name() != m6ManifestName {
			return nil
		}
		snapshot, err := snapshotM6Manifest(repositoryRoot, path)
		if err != nil {
			return err
		}
		validationErr := validate(filepath.Dir(path))
		if validationErr == nil {
			validationErr = validateM7M6Snapshots([]m7M6ManifestSnapshot{snapshot})
		}
		if validationErr != nil {
			if snapshot.SHA256 == expectedCanonicalSHA256 {
				return fmt.Errorf("M6 manifest with expected canonical SHA256 is invalid: %w", validationErr)
			}
			return nil
		}
		snapshots = append(snapshots, snapshot)
		if snapshot.SHA256 == expectedCanonicalSHA256 {
			canonicalExists = true
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	slices.SortFunc(snapshots, func(left, right m7M6ManifestSnapshot) int { return strings.Compare(left.Path, right.Path) })
	if snapshots == nil {
		snapshots = []m7M6ManifestSnapshot{}
	}
	if requireCanonical && !canonicalExists {
		return nil, false, errors.New("canonical M6 manifest is required but no valid manifest has the expected SHA256")
	}
	return snapshots, canonicalExists, nil
}

func snapshotM6Manifest(repositoryRoot, path string) (m7M6ManifestSnapshot, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return m7M6ManifestSnapshot{}, fmt.Errorf("inspect M6 manifest for preservation: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return m7M6ManifestSnapshot{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, io.LimitReader(file, m7MaxManifestBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil || statErr != nil || closeErr != nil {
		return m7M6ManifestSnapshot{}, errors.Join(copyErr, statErr, closeErr)
	}
	if size > m7MaxManifestBytes || size != before.Size() || !os.SameFile(before, after) {
		return m7M6ManifestSnapshot{}, errors.New("M6 manifest changed while snapshotting")
	}
	relative, err := filepath.Rel(repositoryRoot, path)
	if err != nil {
		return m7M6ManifestSnapshot{}, err
	}
	relative = filepath.ToSlash(relative)
	if !strings.HasPrefix(relative, ".cache/integration/m6/") {
		return m7M6ManifestSnapshot{}, errors.New("M6 manifest preservation path is not repository-relative")
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		return m7M6ManifestSnapshot{}, errors.New("M6 manifest inode is unavailable")
	}
	return m7M6ManifestSnapshot{Path: relative, Mode: uint32(after.Mode().Perm()), Size: size, Inode: stat.Ino, ModifiedUnixNano: after.ModTime().UnixNano(), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func requireSameM6Snapshots(before, after []m7M6ManifestSnapshot) error {
	if before == nil || after == nil || !reflect.DeepEqual(before, after) {
		return errors.New("existing valid M6 manifest set, metadata, or SHA256 changed")
	}
	return nil
}

func findM7SharedSecond(samples []monitor.ResourceSample) (uint64, error) {
	task := make(map[uint64]struct{})
	harness := make(map[uint64]struct{})
	for _, sample := range samples {
		if sample.Component == "task-container" {
			task[sample.Second] = struct{}{}
		}
		if sample.Component == "openclaw-harness" {
			harness[sample.Second] = struct{}{}
		}
	}
	shared := make([]uint64, 0)
	for second := range task {
		if _, ok := harness[second]; ok {
			shared = append(shared, second)
		}
	}
	if len(shared) != 0 {
		slices.Sort(shared)
		return shared[0], nil
	}
	return 0, errors.New("monitor has no shared sandbox/harness sample second")
}

func isM7FiniteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func assertPrivateSecretFreeM7Tree(t *testing.T, root, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatal("M7 secret scan requires a nonempty canary")
	}
	files := 0
	var aggregate int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("M7 artifact directory %q has non-private mode %04o", path, info.Mode().Perm())
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("M7 artifact path %q is not a regular file", path)
		}
		files++
		if files > m7MaxArtifacts+1 {
			return fmt.Errorf("M7 secret scan file count exceeds %d", m7MaxArtifacts+1)
		}
		maximum := m7MaxArtifactBytes
		if filepath.Base(path) == m7ManifestName && filepath.Dir(path) == root {
			maximum = m7MaxManifestBytes
		}
		if info.Size() < 0 || info.Size() > maximum || info.Size() > m7MaxArtifactTotalBytes+m7MaxManifestBytes-aggregate {
			return fmt.Errorf("M7 artifact %q exceeds secret-scan bounds", path)
		}
		aggregate += info.Size()
		if info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o044 != 0 && filepath.Base(path) != "aries-exec-helper" {
			return fmt.Errorf("M7 artifact file %q has non-private mode %04o", path, info.Mode().Perm())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return consumeStableM7File(root, filepath.ToSlash(relative), maximum, info.Mode().Perm(), func(reader io.Reader) error {
			found, err := m7ReaderContains(reader, []byte(secret))
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("model credential persisted in %q", path)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func m7ReaderContains(reader io.Reader, needle []byte) (bool, error) {
	if len(needle) == 0 {
		return true, nil
	}
	buffer := make([]byte, 64<<10)
	tail := make([]byte, 0, len(needle)-1)
	for {
		count, err := reader.Read(buffer)
		if count != 0 {
			window := make([]byte, 0, len(tail)+count)
			window = append(window, tail...)
			window = append(window, buffer[:count]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			keep := min(len(needle)-1, len(window))
			tail = append(tail[:0], window[len(window)-keep:]...)
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func testM7Manifest(t *testing.T, root string) m7Manifest {
	t.Helper()
	paths := []string{
		"fake-model-evidence/ready", "fake-model-evidence/status.json", "fake-model-evidence/transcript.jsonl",
		"fix-git/evaluation/ctrf.json", "fix-git/evaluation/reward.txt", "fix-git/evaluation/stderr.log", "fix-git/evaluation/stdout.log",
		"harnesses/test/agent.json", "harnesses/test/agent.stderr", "harnesses/test/gateway.log", "harnesses/test/telemetry.index.json", "harnesses/test/telemetry/test.trajectory.jsonl",
		"m7/cleanup-inventory.json", "m7/git-filesystem-delta.json", "m7/isolation-trace.json", "m7/model-protocol.jsonl",
		"sandboxes/test/container.stderr.log", "sandboxes/test/container.stdout.log", "sandboxes/test/helper/aries-exec-helper",
	}
	for _, relative := range paths {
		if err := writeM6Artifact(root, relative, []byte(relative+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	samples := []monitor.ResourceSample{
		{Sequence: 0, Second: 2, Time: "2026-01-01T00:00:02Z", TaskID: "fix-git", Component: "task-container", ContainerID: "task", ContainerName: "task", CPUPercent: 1, MemoryBytes: 2, MemoryLimitBytes: 10},
		{Sequence: 1, Second: 2, Time: "2026-01-01T00:00:02Z", TaskID: "fix-git", Component: "openclaw-harness", ContainerID: "harness", ContainerName: "harness", CPUPercent: 1, MemoryBytes: 3, MemoryLimitBytes: 10},
	}
	var jsonl bytes.Buffer
	for _, sample := range samples {
		content, _ := json.Marshal(sample)
		jsonl.Write(content)
		jsonl.WriteByte('\n')
	}
	if err := writeM6Artifact(root, "monitor/fix-git/resources.jsonl", jsonl.Bytes()); err != nil {
		t.Fatal(err)
	}
	index := monitor.Index{SchemaVersion: 1, RunID: "run", TaskID: "fix-git", Status: core.StatusSucceeded, StartedAt: "2026-01-01T00:00:00Z", StoppedAt: "2026-01-01T00:00:03Z", DurationNanoseconds: int64(3 * time.Second), IntervalMilliseconds: 1000, SampleCount: 2, ResourcesFile: "resources.jsonl", Components: []monitor.ComponentCoverage{{Component: "openclaw-harness", ContainerID: "harness", ContainerName: "harness", SampleCount: 1, FirstSecond: 2, LastSecond: 2}, {Component: "task-container", ContainerID: "task", ContainerName: "task", SampleCount: 1, FirstSecond: 2, LastSecond: 2}}}
	if err := writeM6JSONArtifact(root, "monitor/fix-git/index.json", index); err != nil {
		t.Fatal(err)
	}
	live := m7LiveValidationArtifact{Status: "not_requested", Attempts: 0, Reason: "deterministic_fake_model"}
	if err := writeM6JSONArtifact(root, "m7/live-validation.json", live); err != nil {
		t.Fatal(err)
	}
	canonicalSnapshot := m7M6ManifestSnapshot{
		Path: ".cache/integration/m6/fix-git-canonical/manifest.json", Mode: 0o400, Size: 1, Inode: 1, ModifiedUnixNano: 1, SHA256: m7CanonicalM6SHA256,
	}
	preservation := m7M6PreservationArtifact{
		Before: []m7M6ManifestSnapshot{canonicalSnapshot}, After: []m7M6ManifestSnapshot{canonicalSnapshot},
	}
	if err := writeM6JSONArtifact(root, "m7/m6-preservation.json", preservation); err != nil {
		t.Fatal(err)
	}
	harnessRefs := []m6ArtifactReference{{Role: m6RoleOpenClawAgentResult, Path: "harnesses/test/agent.json"}, {Role: m6RoleOpenClawAgentStderr, Path: "harnesses/test/agent.stderr"}, {Role: m6RoleOpenClawGatewayLog, Path: "harnesses/test/gateway.log"}, {Role: m6RoleOpenClawTelemetryIndex, Path: "harnesses/test/telemetry.index.json"}}
	evalRefs := []m6ArtifactReference{{Role: m6RoleVerifierCTRF, Path: "fix-git/evaluation/ctrf.json"}, {Role: m6RoleVerifierReward, Path: "fix-git/evaluation/reward.txt"}, {Role: m6RoleVerifierStderr, Path: "fix-git/evaluation/stderr.log"}, {Role: m6RoleVerifierStdout, Path: "fix-git/evaluation/stdout.log"}}
	observerRefs := []m6ArtifactReference{{Role: m7RoleMonitorIndex, Path: "monitor/fix-git/index.json"}, {Role: m7RoleMonitorResources, Path: "monitor/fix-git/resources.jsonl"}}
	outcome := m7PortableTaskResult{TaskID: "fix-git", Harness: m6PortableHarnessResult{Status: core.StatusSucceeded, Artifacts: harnessRefs}, Isolation: m6PortableIsolationResult{Status: core.StatusConfirmed, HarnessStopped: true, BridgeRevoked: true}, Evaluation: m6PortableEvaluationResult{Status: core.StatusSucceeded, Score: 1, Reward: 1, VerifierStatus: core.StatusSucceeded, Artifacts: evalRefs}, Observer: m7PortableObserverResult{Status: core.StatusSucceeded, Duration: 3 * time.Second, SampleCount: 2, Artifacts: observerRefs}, Cleanup: m6PortableStatusResult{Status: core.StatusSucceeded}}
	runResult := m7PortableRunResult{Name: "test", RunID: "run", Tasks: []m7PortableTaskResult{outcome}, Summary: successfulM6RunSummary()}
	if err := writeM6JSONArtifact(root, "m7/run-result.json", runResult); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectM7ArtifactEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	return m7Manifest{SchemaVersion: m7SchemaVersion, RunID: "run", TaskID: "fix-git", Pins: m6Pins{TerminalBenchRevision: terminalbench.Revision, OpenClawImage: PinnedImage, TaskImage: m6FixGitTaskImage, Model: "aries-deterministic"}, Outcomes: outcome, RunResult: m6ArtifactReference{Role: m6RoleRunResult, Path: "m7/run-result.json"}, Observer: m7ObserverEvidence{Status: core.StatusSucceeded, IntervalMilliseconds: 1000, SampleCount: 2, SharedSecond: 2, Resources: observerRefs[1], Index: observerRefs[0]}, Verifier: m6VerifierEvidence{RewardBytes: "1\n", Cases: slices.Clone(m6ExpectedVerifierCases), Sources: slices.Clone(m6ExpectedVerifierSources)}, LiveValidation: m7LiveValidationEvidence{Status: live.Status, Attempts: live.Attempts, Reason: live.Reason, Artifact: m6ArtifactReference{Role: "live_validation", Path: "m7/live-validation.json"}}, M6Preservation: m7M6PreservationEvidence{ManifestCount: 1, CanonicalExists: true, CanonicalSHA256: m7CanonicalM6SHA256, Artifact: m6ArtifactReference{Role: "m6_preservation", Path: "m7/m6-preservation.json"}}, Artifacts: artifacts, Inventory: m6ResourceInventory{Containers: []string{}, Volumes: []string{}, Networks: []string{}}}
}

func TestM7ManifestExclusiveReadonlyStrictRoundTrip(t *testing.T) {
	root := t.TempDir()
	manifest := testM7Manifest(t, root)
	if err := writeM7Manifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	readBack, err := readM7Manifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readBack, manifest) {
		t.Fatal("M7 manifest changed on strict round trip")
	}
	if err := writeM7Manifest(root, manifest); err == nil {
		t.Fatal("M7 manifest overwrite was accepted")
	}
}

func TestM7ArtifactBounds(t *testing.T) {
	writeFiles := func(t *testing.T, files map[string]string) string {
		t.Helper()
		root := t.TempDir()
		for relative, content := range files {
			path := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	t.Run("exact count per-file and aggregate boundaries", func(t *testing.T) {
		root := writeFiles(t, map[string]string{
			"fake-model-evidence/ready":       "1234",
			"fake-model-evidence/status.json": "5678",
		})
		entries, err := collectM7ArtifactEntriesWithLimits(root, m7ArtifactLimits{Count: 2, PerArtifact: 4, AggregateBytes: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Size != 4 || entries[1].Size != 4 {
			t.Fatalf("boundary entries = %#v", entries)
		}
	})
	t.Run("too many artifacts", func(t *testing.T) {
		root := writeFiles(t, map[string]string{
			"fake-model-evidence/ready":            "1",
			"fake-model-evidence/status.json":      "2",
			"fake-model-evidence/transcript.jsonl": "3",
		})
		if _, err := collectM7ArtifactEntriesWithLimits(root, m7ArtifactLimits{Count: 2, PerArtifact: 4, AggregateBytes: 12}); err == nil {
			t.Fatal("artifact count overflow was accepted")
		}
	})
	t.Run("oversized artifact", func(t *testing.T) {
		root := writeFiles(t, map[string]string{"fake-model-evidence/ready": "12345"})
		if _, err := collectM7ArtifactEntriesWithLimits(root, m7ArtifactLimits{Count: 1, PerArtifact: 4, AggregateBytes: 8}); err == nil {
			t.Fatal("per-artifact overflow was accepted")
		}
	})
	t.Run("aggregate overflow", func(t *testing.T) {
		root := writeFiles(t, map[string]string{
			"fake-model-evidence/ready":       "1234",
			"fake-model-evidence/status.json": "5678",
		})
		if _, err := collectM7ArtifactEntriesWithLimits(root, m7ArtifactLimits{Count: 2, PerArtifact: 4, AggregateBytes: 7}); err == nil {
			t.Fatal("artifact aggregate overflow was accepted")
		}
	})
}

type m7EOFMutationFile struct {
	m7ReadStatCloser
	mutate             func() error
	retryAfterMutation bool
	mutated            bool
}

func (file *m7EOFMutationFile) Read(buffer []byte) (int, error) {
	count, err := file.m7ReadStatCloser.Read(buffer)
	if !errors.Is(err, io.EOF) || file.mutated {
		return count, err
	}
	file.mutated = true
	if mutationErr := file.mutate(); mutationErr != nil {
		return 0, mutationErr
	}
	if file.retryAfterMutation {
		return file.m7ReadStatCloser.Read(buffer)
	}
	return count, err
}

func m7MutatingFileOperations(target string, mutate func() error, retry bool) m7FileOperations {
	operations := defaultM7FileOperations()
	open := operations.open
	operations.open = func(path string) (m7ReadStatCloser, error) {
		file, err := open(path)
		if err != nil || path != target {
			return file, err
		}
		return &m7EOFMutationFile{m7ReadStatCloser: file, mutate: mutate, retryAfterMutation: retry}, nil
	}
	return operations
}

func TestM7StableStreamSuccess(t *testing.T) {
	root := t.TempDir()
	relative := "fake-model-evidence/ready"
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("stable-stream\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := hashM7Artifact(root, relative, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if artifact.Size != int64(len(content)) || artifact.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("stable artifact = %#v", artifact)
	}
	hash := sha256.New()
	if err := consumeStableM7File(root, relative, int64(len(content)), 0o600, func(reader io.Reader) error {
		_, err := io.Copy(hash, reader)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash.Sum(nil), digest[:]) {
		t.Fatal("stable stream consumer observed different bytes")
	}
}

func TestM7StableStreamsRejectDeterministicChanges(t *testing.T) {
	type target struct {
		name string
		run  func(string, string, int64, m7FileOperations) error
	}
	targets := []target{
		{name: "hash", run: func(root, relative string, maximum int64, operations m7FileOperations) error {
			_, err := hashM7ArtifactWithOperations(root, relative, maximum, operations)
			return err
		}},
		{name: "consume", run: func(root, relative string, maximum int64, operations m7FileOperations) error {
			return consumeStableM7FileWithOperations(root, relative, maximum, 0o600, func(reader io.Reader) error {
				_, err := io.Copy(io.Discard, reader)
				return err
			}, operations)
		}},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			for name, testCase := range map[string]struct {
				maximum int64
				retry   bool
				mutate  func(string, []byte) func() error
			}{
				"limit plus one growth": {
					maximum: 4, retry: true,
					mutate: func(path string, _ []byte) func() error {
						return func() error { return appendM7TestByte(path) }
					},
				},
				"size change within bound": {
					maximum: 64, retry: true,
					mutate: func(path string, _ []byte) func() error {
						return func() error { return appendM7TestByte(path) }
					},
				},
				"identity change": {
					maximum: 64,
					mutate: func(path string, content []byte) func() error {
						return func() error {
							if err := os.Rename(path, path+".replaced"); err != nil {
								return err
							}
							return os.WriteFile(path, content, 0o600)
						}
					},
				},
				"mtime change": {
					maximum: 64,
					mutate: func(path string, _ []byte) func() error {
						return func() error {
							info, err := os.Stat(path)
							if err != nil {
								return err
							}
							changed := info.ModTime().Add(2 * time.Second)
							return os.Chtimes(path, changed, changed)
						}
					},
				},
			} {
				t.Run(name, func(t *testing.T) {
					root := t.TempDir()
					relative := "fake-model-evidence/ready"
					path := filepath.Join(root, filepath.FromSlash(relative))
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					content := []byte("1234")
					if err := os.WriteFile(path, content, 0o600); err != nil {
						t.Fatal(err)
					}
					operations := m7MutatingFileOperations(path, testCase.mutate(path, content), testCase.retry)
					if err := target.run(root, relative, testCase.maximum, operations); err == nil {
						t.Fatal("changed M7 file was accepted")
					}
				})
			}
		})
	}
}

func appendM7TestByte(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write([]byte("x"))
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func TestM7ManifestRejectsUnknownFieldsAndResourceInvariantBreaks(t *testing.T) {
	t.Run("unknown manifest field", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, m7ManifestName), []byte(`{"schema_version":"aries.m7.monitored.v1","unknown":true}`), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := readM7Manifest(root); err == nil {
			t.Fatal("unknown M7 manifest field was accepted")
		}
	})
	for name, mutate := range map[string]func(*m7Manifest){
		"interval":           func(manifest *m7Manifest) { manifest.Observer.IntervalMilliseconds = 999 },
		"shared second":      func(manifest *m7Manifest) { manifest.Observer.SharedSecond = 99 },
		"sample count":       func(manifest *m7Manifest) { manifest.Observer.SampleCount++ },
		"artifact path":      func(manifest *m7Manifest) { manifest.Artifacts[0].Path = "../escape" },
		"self reference":     func(manifest *m7Manifest) { manifest.Artifacts[0].Path = m7ManifestName },
		"duplicate artifact": func(manifest *m7Manifest) { manifest.Artifacts = append(manifest.Artifacts, manifest.Artifacts[0]) },
		"canonical false":    func(manifest *m7Manifest) { manifest.M6Preservation.CanonicalExists = false },
		"canonical hash":     func(manifest *m7Manifest) { manifest.M6Preservation.CanonicalSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := testM7Manifest(t, root)
			mutate(&manifest)
			if err := validateM7Manifest(root, manifest); err == nil {
				t.Fatal("invalid M7 manifest was accepted")
			}
		})
	}
	t.Run("canonical missing from preservation artifact", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM7Manifest(t, root)
		content, err := json.Marshal(m7M6PreservationArtifact{Before: []m7M6ManifestSnapshot{}, After: []m7M6ManifestSnapshot{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "m7", "m6-preservation.json"), append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts, err = collectM7ArtifactEntries(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateM7Manifest(root, manifest); err == nil {
			t.Fatal("M7 preservation artifact without the canonical M6 manifest was accepted")
		}
	})
	t.Run("artifact symlink", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM7Manifest(t, root)
		if err := os.Symlink("ready", filepath.Join(root, "fake-model-evidence", "linked")); err != nil {
			t.Fatal(err)
		}
		if err := validateM7Manifest(root, manifest); err == nil {
			t.Fatal("M7 artifact symlink was accepted")
		}
	})
	t.Run("manifest trailing content", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, m7ManifestName), []byte("{}\n{}\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := readM7Manifest(root); err == nil {
			t.Fatal("M7 manifest trailing content was accepted")
		}
	})
	t.Run("oversized manifest", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, m7ManifestName), bytes.Repeat([]byte{'x'}, int(m7MaxManifestBytes)+1), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := readM7Manifest(root); err == nil {
			t.Fatal("oversized M7 manifest was accepted")
		}
	})
}

func TestM7CanonicalRequirementEnvironment(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		prior, existed := os.LookupEnv(m7RequireCanonicalM6Env)
		if err := os.Unsetenv(m7RequireCanonicalM6Env); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(m7RequireCanonicalM6Env, prior)
			} else {
				_ = os.Unsetenv(m7RequireCanonicalM6Env)
			}
		})
		got, err := requireCanonicalM6FromEnv()
		if err != nil || got {
			t.Fatalf("unset canonical requirement = %v, %v", got, err)
		}
	})
	for _, testCase := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "empty", value: ""},
		{name: "enabled", value: "1", want: true},
		{name: "zero", value: "0", wantErr: true},
		{name: "boolean", value: "true", wantErr: true},
		{name: "whitespace", value: " 1", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(m7RequireCanonicalM6Env, testCase.value)
			got, err := requireCanonicalM6FromEnv()
			if (err != nil) != testCase.wantErr || got != testCase.want {
				t.Fatalf("canonical requirement = %v, %v; want %v, error=%v", got, err, testCase.want, testCase.wantErr)
			}
		})
	}
}

func TestM7ManifestUsesCanonicalRequirement(t *testing.T) {
	t.Run("expected hash present", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM7Manifest(t, root)
		for _, value := range []string{"", "1"} {
			t.Setenv(m7RequireCanonicalM6Env, value)
			if err := validateM7Manifest(root, manifest); err != nil {
				t.Fatalf("canonical requirement %q rejected expected manifest: %v", value, err)
			}
		}
	})
	t.Run("empty preserved set", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM7Manifest(t, root)
		preservation := m7M6PreservationArtifact{Before: []m7M6ManifestSnapshot{}, After: []m7M6ManifestSnapshot{}}
		content, err := json.Marshal(preservation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "m7", "m6-preservation.json"), append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest.M6Preservation.ManifestCount = 0
		manifest.M6Preservation.CanonicalExists = false
		manifest.M6Preservation.CanonicalSHA256 = ""
		manifest.Artifacts, err = collectM7ArtifactEntries(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(m7RequireCanonicalM6Env, "")
		if err := validateM7Manifest(root, manifest); err != nil {
			t.Fatalf("default policy rejected an empty preserved set: %v", err)
		}
		t.Setenv(m7RequireCanonicalM6Env, "1")
		if err := validateM7Manifest(root, manifest); err == nil {
			t.Fatal("required policy accepted an empty preserved set")
		}
	})
}

func TestM7PreservationCanonicalPolicy(t *testing.T) {
	expected := m7M6ManifestSnapshot{
		Path: ".cache/integration/m6/expected/manifest.json", Mode: 0o400, Size: 1,
		Inode: 1, ModifiedUnixNano: 1, SHA256: m7CanonicalM6SHA256,
	}
	different := expected
	different.Path = ".cache/integration/m6/different/manifest.json"
	different.SHA256 = strings.Repeat("a", 64)
	invalid := different
	invalid.Mode = 0o600
	for _, testCase := range []struct {
		name             string
		snapshots        []m7M6ManifestSnapshot
		canonicalExists  bool
		canonicalSHA256  string
		requireCanonical bool
		wantErr          bool
	}{
		{name: "empty default", snapshots: []m7M6ManifestSnapshot{}},
		{name: "empty required", snapshots: []m7M6ManifestSnapshot{}, requireCanonical: true, wantErr: true},
		{name: "expected default", snapshots: []m7M6ManifestSnapshot{expected}, canonicalExists: true, canonicalSHA256: m7CanonicalM6SHA256},
		{name: "expected required", snapshots: []m7M6ManifestSnapshot{expected}, canonicalExists: true, canonicalSHA256: m7CanonicalM6SHA256, requireCanonical: true},
		{name: "different default", snapshots: []m7M6ManifestSnapshot{different}},
		{name: "different required", snapshots: []m7M6ManifestSnapshot{different}, requireCanonical: true, wantErr: true},
		{name: "false despite expected", snapshots: []m7M6ManifestSnapshot{expected}, canonicalSHA256: m7CanonicalM6SHA256, wantErr: true},
		{name: "true without expected", snapshots: []m7M6ManifestSnapshot{different}, canonicalExists: true, canonicalSHA256: m7CanonicalM6SHA256, wantErr: true},
		{name: "hash without expected", snapshots: []m7M6ManifestSnapshot{different}, canonicalSHA256: m7CanonicalM6SHA256, wantErr: true},
		{name: "invalid snapshot", snapshots: []m7M6ManifestSnapshot{invalid}, wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			artifact := m7M6PreservationArtifact{Before: slices.Clone(testCase.snapshots), After: slices.Clone(testCase.snapshots)}
			if err := writeM6JSONArtifact(root, "m7/m6-preservation.json", artifact); err != nil {
				t.Fatal(err)
			}
			reference := m6ArtifactReference{Role: "m6_preservation", Path: "m7/m6-preservation.json"}
			evidence := m7M6PreservationEvidence{
				ManifestCount: len(testCase.snapshots), CanonicalExists: testCase.canonicalExists,
				CanonicalSHA256: testCase.canonicalSHA256, Artifact: reference,
			}
			artifacts := map[string]m7ArtifactEntry{reference.Path: {Role: reference.Role, Path: reference.Path}}
			err := validateM7Preservation(root, evidence, artifacts, testCase.requireCanonical)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validate preservation error = %v, want error=%v", err, testCase.wantErr)
			}
		})
	}
}

func TestM7M6SnapshotPreservationDetectsSetMetadataAndContentChanges(t *testing.T) {
	repository := t.TempDir()
	first := filepath.Join(repository, ".cache", "integration", "m6", "one", m6ManifestName)
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("one\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	validator := func(string) error { return nil }
	expectedCanonicalSHA256 := strings.Repeat("f", 64)
	before, canonical, err := snapshotM6ManifestsWithValidator(repository, validator, false, expectedCanonicalSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if canonical {
		t.Fatal("unexpected canonical M6 manifest")
	}
	after, _, err := snapshotM6ManifestsWithValidator(repository, validator, false, expectedCanonicalSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSameM6Snapshots(before, after); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(repository, ".cache", "integration", "m6", "two", m6ManifestName)
	if err := os.MkdirAll(filepath.Dir(second), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	changedSet, _, err := snapshotM6ManifestsWithValidator(repository, validator, false, expectedCanonicalSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSameM6Snapshots(before, changedSet); err == nil {
		t.Fatal("changed M6 manifest set was accepted")
	}
	if err := os.Chmod(first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first, 0o400); err != nil {
		t.Fatal(err)
	}
	changedContent, _, err := snapshotM6ManifestsWithValidator(repository, validator, false, expectedCanonicalSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(changedContent) == 0 || reflect.DeepEqual(before[0], changedContent[0]) {
		t.Fatal("changed M6 metadata/content was not detected")
	}
}

func TestM7SnapshotCanonicalPolicy(t *testing.T) {
	validator := func(string) error { return nil }
	t.Run("empty default", func(t *testing.T) {
		snapshots, canonical, err := snapshotM6Manifests(t.TempDir(), false)
		if err != nil || len(snapshots) != 0 || canonical {
			t.Fatalf("empty default snapshot = %#v, %v, %v", snapshots, canonical, err)
		}
	})
	t.Run("empty required", func(t *testing.T) {
		if _, _, err := snapshotM6Manifests(t.TempDir(), true); err == nil {
			t.Fatal("required canonical M6 snapshot accepted an empty repository")
		}
	})
	t.Run("expected hash present", func(t *testing.T) {
		repository, expected := writeM7SnapshotTestManifest(t, "canonical\n", 0o400)
		for _, required := range []bool{false, true} {
			snapshots, canonical, err := snapshotM6ManifestsWithValidator(repository, validator, required, expected)
			if err != nil || len(snapshots) != 1 || !canonical || snapshots[0].SHA256 != expected {
				t.Fatalf("required=%v snapshot = %#v, %v, %v", required, snapshots, canonical, err)
			}
		}
	})
	t.Run("different hash", func(t *testing.T) {
		repository, _ := writeM7SnapshotTestManifest(t, "different\n", 0o400)
		snapshots, canonical, err := snapshotM6ManifestsWithValidator(repository, validator, false, m7CanonicalM6SHA256)
		if err != nil || len(snapshots) != 1 || canonical {
			t.Fatalf("default different-hash snapshot = %#v, %v, %v", snapshots, canonical, err)
		}
		if _, _, err := snapshotM6ManifestsWithValidator(repository, validator, true, m7CanonicalM6SHA256); err == nil {
			t.Fatal("required snapshot accepted a different hash")
		}
	})
	t.Run("expected hash fails M6 validation", func(t *testing.T) {
		repository, expected := writeM7SnapshotTestManifest(t, "invalid canonical\n", 0o400)
		invalid := func(string) error { return errors.New("invalid M6 manifest") }
		if _, _, err := snapshotM6ManifestsWithValidator(repository, invalid, false, expected); err == nil {
			t.Fatal("expected canonical hash that failed M6 validation was skipped")
		}
	})
	t.Run("noncanonical invalid manifest is skipped", func(t *testing.T) {
		repository, _ := writeM7SnapshotTestManifest(t, "invalid noncanonical\n", 0o400)
		invalid := func(string) error { return errors.New("invalid M6 manifest") }
		snapshots, canonical, err := snapshotM6ManifestsWithValidator(repository, invalid, false, m7CanonicalM6SHA256)
		if err != nil || len(snapshots) != 0 || canonical {
			t.Fatalf("invalid noncanonical snapshot = %#v, %v, %v", snapshots, canonical, err)
		}
	})
	t.Run("expected hash with invalid metadata", func(t *testing.T) {
		repository, expected := writeM7SnapshotTestManifest(t, "wrong mode canonical\n", 0o600)
		if _, _, err := snapshotM6ManifestsWithValidator(repository, validator, false, expected); err == nil {
			t.Fatal("expected canonical hash with invalid metadata was skipped")
		}
	})
}

func writeM7SnapshotTestManifest(t *testing.T, content string, mode os.FileMode) (string, string) {
	t.Helper()
	repository := t.TempDir()
	path := filepath.Join(repository, ".cache", "integration", "m6", "test", m6ManifestName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return repository, hex.EncodeToString(digest[:])
}

func TestM7M6SnapshotMetadataIsStrict(t *testing.T) {
	valid := m7M6ManifestSnapshot{
		Path: ".cache/integration/m6/fix-git-1/manifest.json", Mode: 0o400,
		Size: 10, Inode: 1, ModifiedUnixNano: 1, SHA256: strings.Repeat("a", 64),
	}
	if err := validateM7M6Snapshots([]m7M6ManifestSnapshot{valid}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*m7M6ManifestSnapshot){
		"escape": func(snapshot *m7M6ManifestSnapshot) { snapshot.Path = "../manifest.json" },
		"mode":   func(snapshot *m7M6ManifestSnapshot) { snapshot.Mode = 0o600 },
		"hash":   func(snapshot *m7M6ManifestSnapshot) { snapshot.SHA256 = "not-a-digest" },
		"inode":  func(snapshot *m7M6ManifestSnapshot) { snapshot.Inode = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			if err := validateM7M6Snapshots([]m7M6ManifestSnapshot{invalid}); err == nil {
				t.Fatal("invalid M6 preservation metadata was accepted")
			}
		})
	}
}
