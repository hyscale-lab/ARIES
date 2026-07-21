package openclaw

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	m6ManifestName      = "manifest.json"
	m6SchemaVersion     = "aries.m6.oracle.v1"
	m6FixGitTaskImage   = "alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119"
	m6MaxManifestBytes  = int64(1 << 20)
	m6MaxRunResultBytes = int64(1 << 20)

	m6RoleOpenClawAgentResult    = "openclaw_agent_result"
	m6RoleOpenClawAgentStderr    = "openclaw_agent_stderr"
	m6RoleOpenClawGatewayLog     = "openclaw_gateway_log"
	m6RoleOpenClawTelemetryIndex = "openclaw_telemetry_index"
	m6RoleVerifierStdout         = "verifier_stdout"
	m6RoleVerifierStderr         = "verifier_stderr"
	m6RoleVerifierCTRF           = "verifier_ctrf"
	m6RoleVerifierReward         = "verifier_reward"
	m6RoleRunResult              = "run_result"
)

var m6ExpectedVerifierCases = []m6VerifierCase{
	{Name: "test_about_file", Status: "passed"},
	{Name: "test_layout_file", Status: "passed"},
}

var m6ExpectedVerifierSources = []m6SourceHash{
	{Path: "fix-git/tests/test.sh", SHA256: "4770437ea96c3cc84684b4f99d55fb148fcac09f9ea1e8ef49de487716e6c334"},
	{Path: "fix-git/tests/test_outputs.py", SHA256: "a8a6f51eb0d7c4ebdf0813c8f0b645edf29c995f1f9faf2aca3886a6713043f3"},
}

var m6RequiredFixedArtifacts = []string{
	"fix-git/evaluation/ctrf.json",
	"fix-git/evaluation/reward.txt",
	"fix-git/evaluation/stderr.log",
	"fix-git/evaluation/stdout.log",
	"m6/cleanup-inventory.json",
	"m6/git-filesystem-delta.json",
	"m6/isolation-trace.json",
	"m6/model-protocol.jsonl",
	"m6/run-result.json",
}

type m6Manifest struct {
	SchemaVersion string               `json:"schema_version"`
	RunID         string               `json:"run_id"`
	TaskID        string               `json:"task_id"`
	Pins          m6Pins               `json:"pins"`
	Outcomes      m6PortableTaskResult `json:"outcomes"`
	RunResult     m6ArtifactReference  `json:"run_result"`
	Observer      m6ObserverEvidence   `json:"observer"`
	Verifier      m6VerifierEvidence   `json:"verifier"`
	Artifacts     []m6ArtifactEntry    `json:"artifacts"`
	Inventory     m6ResourceInventory  `json:"resource_inventory"`
}

// m6PortableRunResult is an evidence-only copy of core.RunResult. It retains
// outcome data while replacing host paths with typed, root-relative artifact
// references. The Runner-owned result is never modified.
type m6PortableRunResult struct {
	Name     string                 `json:"name"`
	RunID    string                 `json:"run_id"`
	Tasks    []m6PortableTaskResult `json:"tasks"`
	Summary  m6PortableRunSummary   `json:"summary"`
	Duration time.Duration          `json:"duration"`
}

type m6PortableTaskResult struct {
	TaskID     string                     `json:"task_id"`
	Harness    m6PortableHarnessResult    `json:"harness"`
	Isolation  m6PortableIsolationResult  `json:"isolation"`
	Evaluation m6PortableEvaluationResult `json:"evaluation"`
	Observer   m6PortableStatusResult     `json:"observer"`
	Cleanup    m6PortableStatusResult     `json:"cleanup"`
	Duration   time.Duration              `json:"duration"`
}

type m6PortableHarnessResult struct {
	Status        string                `json:"status"`
	FinalResponse string                `json:"final_response,omitempty"`
	Duration      time.Duration         `json:"duration"`
	Artifacts     []m6ArtifactReference `json:"artifacts"`
	Error         string                `json:"error,omitempty"`
}

type m6PortableIsolationResult struct {
	Status         string `json:"status"`
	HarnessStopped bool   `json:"harness_stopped"`
	BridgeRevoked  bool   `json:"bridge_revoked"`
	Error          string `json:"error,omitempty"`
}

type m6PortableEvaluationResult struct {
	Status         string                `json:"status"`
	Score          float64               `json:"score"`
	Reward         float64               `json:"reward"`
	VerifierStatus string                `json:"verifier_status"`
	Duration       time.Duration         `json:"duration"`
	Artifacts      []m6ArtifactReference `json:"artifacts"`
	Error          string                `json:"error,omitempty"`
}

type m6PortableStatusResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type m6PortableRunSummary struct {
	Tasks                int `json:"tasks"`
	HarnessSucceeded     int `json:"harness_succeeded"`
	HarnessFailed        int `json:"harness_failed"`
	EvaluationsRun       int `json:"evaluations_run"`
	EvaluationsSucceeded int `json:"evaluations_succeeded"`
	EvaluationsFailed    int `json:"evaluations_failed"`
	EvaluationsBlocked   int `json:"evaluations_blocked"`
	CleanupFailed        int `json:"cleanup_failed"`
}

type m6ArtifactReference struct {
	Role string `json:"role"`
	Path string `json:"path"`
}

type m6Pins struct {
	TerminalBenchRevision string `json:"terminal_bench_revision"`
	OpenClawImage         string `json:"openclaw_image"`
	TaskImage             string `json:"task_image"`
	Model                 string `json:"model"`
}

type m6ObserverEvidence struct {
	Status  string             `json:"status"`
	Samples []m6ObserverSample `json:"samples"`
}

type m6ObserverSample struct {
	Second   int     `json:"second"`
	CPU      float64 `json:"cpu"`
	MemoryMB int     `json:"memory_mb"`
}

type m6VerifierEvidence struct {
	RewardBytes string           `json:"reward_bytes"`
	Cases       []m6VerifierCase `json:"cases"`
	Sources     []m6SourceHash   `json:"source_hashes"`
}

type m6VerifierCase struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type m6SourceHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type m6ArtifactEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type m6ResourceInventory struct {
	Containers []string `json:"containers"`
	Volumes    []string `json:"volumes"`
	Networks   []string `json:"networks"`
}

func newM6PortableRunResult(root string, result core.RunResult) (m6PortableRunResult, error) {
	tasks := make([]m6PortableTaskResult, 0, len(result.Tasks))
	for _, task := range result.Tasks {
		portable, err := newM6PortableTaskResult(root, task)
		if err != nil {
			return m6PortableRunResult{}, err
		}
		tasks = append(tasks, portable)
	}
	return m6PortableRunResult{
		Name: result.Name, RunID: result.RunID, Tasks: tasks,
		Summary: m6PortableRunSummary{
			Tasks: result.Summary.Tasks, HarnessSucceeded: result.Summary.HarnessSucceeded,
			HarnessFailed: result.Summary.HarnessFailed, EvaluationsRun: result.Summary.EvaluationsRun,
			EvaluationsSucceeded: result.Summary.EvaluationsSucceeded, EvaluationsFailed: result.Summary.EvaluationsFailed,
			EvaluationsBlocked: result.Summary.EvaluationsBlocked, CleanupFailed: result.Summary.CleanupFailed,
		},
		Duration: result.Duration,
	}, nil
}

func newM6PortableTaskResult(root string, result core.TaskResult) (m6PortableTaskResult, error) {
	harnessArtifacts, err := newM6ArtifactReferences(root, result.Harness.LogPaths, m6HarnessArtifactRole)
	if err != nil {
		return m6PortableTaskResult{}, fmt.Errorf("normalize harness artifacts: %w", err)
	}
	evaluationArtifacts, err := newM6ArtifactReferences(root, result.Evaluation.LogPaths, m6EvaluationArtifactRole)
	if err != nil {
		return m6PortableTaskResult{}, fmt.Errorf("normalize evaluation artifacts: %w", err)
	}
	return m6PortableTaskResult{
		TaskID: result.TaskID,
		Harness: m6PortableHarnessResult{
			Status: result.Harness.Status, FinalResponse: result.Harness.FinalResponse,
			Duration: result.Harness.Duration, Artifacts: harnessArtifacts, Error: result.Harness.Error,
		},
		Isolation: m6PortableIsolationResult{
			Status: result.Isolation.Status, HarnessStopped: result.Isolation.HarnessStopped,
			BridgeRevoked: result.Isolation.BridgeRevoked, Error: result.Isolation.Error,
		},
		Evaluation: m6PortableEvaluationResult{
			Status: result.Evaluation.Status, Score: result.Evaluation.Score, Reward: result.Evaluation.Reward,
			VerifierStatus: result.Evaluation.VerifierStatus, Duration: result.Evaluation.Duration,
			Artifacts: evaluationArtifacts, Error: result.Evaluation.Error,
		},
		Observer: m6PortableStatusResult{Status: result.Observer.Status, Error: result.Observer.Error},
		Cleanup:  m6PortableStatusResult{Status: result.Cleanup.Status, Error: result.Cleanup.Error},
		Duration: result.Duration,
	}, nil
}

func newM6ArtifactReferences(root string, paths []string, roleForPath func(string) (string, error)) ([]m6ArtifactReference, error) {
	references := make([]m6ArtifactReference, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("artifact path %q is not absolute Runner output", path)
		}
		relative, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil, err
		}
		relative = filepath.ToSlash(relative)
		if _, err := m6ArtifactPath(rootAbs, relative); err != nil {
			return nil, err
		}
		if _, ok := seen[relative]; ok {
			return nil, fmt.Errorf("duplicate portable artifact reference %q", relative)
		}
		seen[relative] = struct{}{}
		role, err := roleForPath(relative)
		if err != nil {
			return nil, err
		}
		references = append(references, m6ArtifactReference{Role: role, Path: relative})
	}
	slices.SortFunc(references, func(left, right m6ArtifactReference) int { return strings.Compare(left.Role, right.Role) })
	if references == nil {
		references = []m6ArtifactReference{}
	}
	return references, nil
}

func m6HarnessArtifactRole(relative string) (string, error) {
	if !strings.HasPrefix(relative, "harnesses/") {
		return "", fmt.Errorf("OpenClaw artifact %q is outside harnesses", relative)
	}
	switch filepath.Base(relative) {
	case "agent.json":
		return m6RoleOpenClawAgentResult, nil
	case "agent.stderr":
		return m6RoleOpenClawAgentStderr, nil
	case "gateway.log":
		return m6RoleOpenClawGatewayLog, nil
	case "telemetry.index.json":
		return m6RoleOpenClawTelemetryIndex, nil
	default:
		return "", fmt.Errorf("unsupported OpenClaw outcome artifact %q", relative)
	}
}

func m6EvaluationArtifactRole(relative string) (string, error) {
	switch relative {
	case "fix-git/evaluation/stdout.log":
		return m6RoleVerifierStdout, nil
	case "fix-git/evaluation/stderr.log":
		return m6RoleVerifierStderr, nil
	case "fix-git/evaluation/ctrf.json":
		return m6RoleVerifierCTRF, nil
	case "fix-git/evaluation/reward.txt":
		return m6RoleVerifierReward, nil
	default:
		return "", fmt.Errorf("unsupported verifier outcome artifact %q", relative)
	}
}

func newM6EvidenceDir(repositoryRoot string) (string, error) {
	root := filepath.Join(repositoryRoot, ".cache", "integration", "m6")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create M6 integration root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("make M6 integration root private: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect M6 integration root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", errors.New("M6 integration root is not a private real directory")
	}
	directory, err := os.MkdirTemp(root, "fix-git-")
	if err != nil {
		return "", fmt.Errorf("create unique M6 evidence directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("make M6 evidence directory private: %w", err)
	}
	return directory, nil
}

func writeM6JSONArtifact(root, relative string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode M6 artifact %q: %w", relative, err)
	}
	return writeM6Artifact(root, relative, append(content, '\n'))
}

func writeM6Artifact(root, relative string, content []byte) error {
	path, err := m6ArtifactPath(root, relative)
	if err != nil {
		return err
	}
	if relative == m6ManifestName {
		return errors.New("manifest must be written by writeM6Manifest")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create M6 artifact parent: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create M6 artifact %q: %w", relative, err)
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write M6 artifact %q: %w", relative, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync M6 artifact %q: %w", relative, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close M6 artifact %q: %w", relative, err)
	}
	written = true
	return nil
}

func collectM6ArtifactEntries(root string) ([]m6ArtifactEntry, error) {
	var entries []m6ArtifactEntry
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("M6 artifact tree contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == m6ManifestName {
			return errors.New("manifest already exists before artifact collection")
		}
		artifact, err := hashM6Artifact(root, relative)
		if err != nil {
			return err
		}
		entries = append(entries, artifact)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(left, right m6ArtifactEntry) int { return strings.Compare(left.Path, right.Path) })
	if entries == nil {
		entries = []m6ArtifactEntry{}
	}
	return entries, nil
}

func writeM6Manifest(root string, manifest m6Manifest) error {
	if err := validateM6Manifest(root, manifest); err != nil {
		return err
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode M6 manifest: %w", err)
	}
	path := filepath.Join(root, m6ManifestName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create M6 manifest exclusively: %w", err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(content, '\n')); err != nil {
		return fmt.Errorf("write M6 manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync M6 manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close M6 manifest: %w", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return fmt.Errorf("make M6 manifest readonly: %w", err)
	}
	readBack, err := readM6Manifest(root)
	if err != nil {
		return err
	}
	if err := validateM6Manifest(root, readBack); err != nil {
		return fmt.Errorf("validate M6 manifest readback: %w", err)
	}
	complete = true
	return nil
}

func readM6Manifest(root string) (m6Manifest, error) {
	path := filepath.Join(root, m6ManifestName)
	info, err := os.Lstat(path)
	if err != nil {
		return m6Manifest{}, fmt.Errorf("inspect M6 manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
		return m6Manifest{}, fmt.Errorf("M6 manifest mode/type = %v/%04o, want regular/0400", info.Mode().Type(), info.Mode().Perm())
	}
	if info.Size() > m6MaxManifestBytes {
		return m6Manifest{}, fmt.Errorf("M6 manifest size %d exceeds maximum %d", info.Size(), m6MaxManifestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return m6Manifest{}, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, m6MaxManifestBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return m6Manifest{}, errors.Join(readErr, statErr, closeErr)
	}
	if !os.SameFile(info, after) || !after.Mode().IsRegular() || after.Size() != info.Size() || int64(len(content)) != info.Size() {
		return m6Manifest{}, errors.New("M6 manifest changed while reading")
	}
	if int64(len(content)) > m6MaxManifestBytes {
		return m6Manifest{}, fmt.Errorf("M6 manifest exceeds maximum %d while reading", m6MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest m6Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return m6Manifest{}, fmt.Errorf("decode M6 manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return m6Manifest{}, errors.New("M6 manifest has trailing JSON content")
	}
	return manifest, nil
}

func validateM6Manifest(root string, manifest m6Manifest) error {
	if manifest.SchemaVersion != m6SchemaVersion || manifest.RunID == "" || manifest.TaskID != "fix-git" {
		return errors.New("M6 manifest schema or identity is invalid")
	}
	if manifest.Pins.TerminalBenchRevision != terminalbench.Revision || manifest.Pins.OpenClawImage != PinnedImage || manifest.Pins.TaskImage != m6FixGitTaskImage || manifest.Pins.Model != "aries-deterministic" {
		return errors.New("M6 manifest pins are not exact")
	}
	outcomes := manifest.Outcomes
	if outcomes.TaskID != manifest.TaskID || outcomes.Harness.Status != core.StatusSucceeded || outcomes.Harness.Error != "" || outcomes.Isolation.Status != core.StatusConfirmed || !outcomes.Isolation.HarnessStopped || !outcomes.Isolation.BridgeRevoked || outcomes.Isolation.Error != "" || outcomes.Evaluation.Status != core.StatusSucceeded || outcomes.Evaluation.VerifierStatus != core.StatusSucceeded || outcomes.Evaluation.Score != 1 || outcomes.Evaluation.Reward != 1 || outcomes.Evaluation.Error != "" || outcomes.Observer.Status != core.StatusNotEnabled || outcomes.Observer.Error != "" || outcomes.Cleanup.Status != core.StatusSucceeded || outcomes.Cleanup.Error != "" {
		return errors.New("M6 manifest outcomes are not the successful separated Runner result")
	}
	if manifest.Observer.Status != core.StatusNotEnabled || manifest.Observer.Samples == nil || len(manifest.Observer.Samples) != 0 {
		return errors.New("M6 observer evidence must be not_enabled with an explicit empty sample list")
	}
	if manifest.Verifier.RewardBytes != "1\n" || !slices.Equal(manifest.Verifier.Cases, m6ExpectedVerifierCases) {
		return errors.New("M6 verifier reward or exact cases are invalid")
	}
	if !slices.Equal(manifest.Verifier.Sources, m6ExpectedVerifierSources) {
		return errors.New("M6 verifier source paths or hashes are not exact")
	}
	if manifest.Artifacts == nil || len(manifest.Artifacts) == 0 {
		return errors.New("M6 manifest artifact list is empty or null")
	}
	seen := make(map[string]m6ArtifactEntry, len(manifest.Artifacts))
	previous := ""
	for _, artifact := range manifest.Artifacts {
		if artifact.Path == m6ManifestName {
			return errors.New("M6 manifest cannot reference itself")
		}
		if _, ok := seen[artifact.Path]; ok {
			return fmt.Errorf("duplicate M6 artifact %q", artifact.Path)
		}
		seen[artifact.Path] = artifact
		if previous != "" && strings.Compare(previous, artifact.Path) >= 0 {
			return errors.New("M6 artifact entries are not strictly sorted")
		}
		previous = artifact.Path
		actual, err := hashM6Artifact(root, artifact.Path)
		if err != nil {
			return err
		}
		if actual != artifact {
			return fmt.Errorf("stale M6 artifact entry %q", artifact.Path)
		}
	}
	if err := validateM6RequiredArtifactSet(seen); err != nil {
		return err
	}
	if err := validateM6OutcomeArtifactReferences(root, outcomes, seen); err != nil {
		return err
	}
	if manifest.RunResult != (m6ArtifactReference{Role: m6RoleRunResult, Path: "m6/run-result.json"}) {
		return fmt.Errorf("M6 run-result reference is not exact: %#v", manifest.RunResult)
	}
	if _, ok := seen[manifest.RunResult.Path]; !ok {
		return errors.New("M6 run-result reference is not linked to a hashed artifact entry")
	}
	portableRunResult, err := readM6PortableRunResult(root, manifest.RunResult.Path)
	if err != nil {
		return err
	}
	if portableRunResult.RunID != manifest.RunID || portableRunResult.Name == "" || len(portableRunResult.Tasks) != 1 || !reflect.DeepEqual(portableRunResult.Tasks[0], outcomes) || portableRunResult.Summary != successfulM6RunSummary() {
		return errors.New("M6 portable run-result artifact does not match the manifest outcome")
	}
	if manifest.Inventory.Containers == nil || manifest.Inventory.Volumes == nil || manifest.Inventory.Networks == nil || len(manifest.Inventory.Containers) != 0 || len(manifest.Inventory.Volumes) != 0 || len(manifest.Inventory.Networks) != 0 {
		return errors.New("M6 exact final resource inventories must be explicit empty lists")
	}
	return nil
}

func validateM6RequiredArtifactSet(seen map[string]m6ArtifactEntry) error {
	for _, required := range m6RequiredFixedArtifacts {
		if _, ok := seen[required]; !ok {
			return fmt.Errorf("M6 manifest is missing required artifact %q", required)
		}
	}
	requiredHarness := map[string]bool{
		"agent.json": false, "agent.stderr": false, "gateway.log": false, "telemetry.index.json": false,
	}
	foundTrajectory := false
	for path := range seen {
		if !strings.HasPrefix(path, "harnesses/") {
			continue
		}
		if _, ok := requiredHarness[filepath.Base(path)]; ok {
			requiredHarness[filepath.Base(path)] = true
		}
		if strings.HasSuffix(path, ".trajectory.jsonl") {
			foundTrajectory = true
		}
	}
	for name, found := range requiredHarness {
		if !found {
			return fmt.Errorf("M6 manifest is missing OpenClaw artifact %q", name)
		}
	}
	if !foundTrajectory {
		return errors.New("M6 manifest is missing an OpenClaw trajectory JSONL")
	}
	return nil
}

func validateM6OutcomeArtifactReferences(root string, outcomes m6PortableTaskResult, artifacts map[string]m6ArtifactEntry) error {
	type referencedFile struct {
		path string
		info os.FileInfo
	}
	seenPaths := make(map[string]struct{}, len(outcomes.Harness.Artifacts)+len(outcomes.Evaluation.Artifacts))
	var referencedFiles []referencedFile
	validate := func(group string, references []m6ArtifactReference, expected map[string]string, dynamicHarnessPath bool) error {
		if references == nil || len(references) != len(expected) {
			return fmt.Errorf("M6 %s artifact references are not exact", group)
		}
		seenRoles := make(map[string]struct{}, len(references))
		previousRole := ""
		for _, reference := range references {
			expectedPath, ok := expected[reference.Role]
			if !ok {
				return fmt.Errorf("M6 %s artifact role %q is unknown", group, reference.Role)
			}
			if _, ok := seenRoles[reference.Role]; ok {
				return fmt.Errorf("M6 %s artifact role %q is duplicated", group, reference.Role)
			}
			seenRoles[reference.Role] = struct{}{}
			if previousRole != "" && strings.Compare(previousRole, reference.Role) >= 0 {
				return fmt.Errorf("M6 %s artifact references are not sorted by role", group)
			}
			previousRole = reference.Role
			path, err := m6ArtifactPath(root, reference.Path)
			if err != nil {
				return fmt.Errorf("M6 %s artifact reference: %w", group, err)
			}
			if dynamicHarnessPath {
				if !strings.HasPrefix(reference.Path, "harnesses/") || filepath.Base(reference.Path) != expectedPath {
					return fmt.Errorf("M6 %s artifact role %q has invalid path %q", group, reference.Role, reference.Path)
				}
			} else if reference.Path != expectedPath {
				return fmt.Errorf("M6 %s artifact role %q has path %q, want %q", group, reference.Role, reference.Path, expectedPath)
			}
			if _, ok := artifacts[reference.Path]; !ok {
				return fmt.Errorf("M6 %s artifact reference %q is not linked to a hashed artifact entry", group, reference.Path)
			}
			if _, ok := seenPaths[reference.Path]; ok {
				return fmt.Errorf("M6 outcome artifact reference %q is duplicated", reference.Path)
			}
			seenPaths[reference.Path] = struct{}{}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			for _, prior := range referencedFiles {
				if os.SameFile(prior.info, info) {
					return fmt.Errorf("M6 outcome artifact paths %q and %q alias the same file", prior.path, reference.Path)
				}
			}
			referencedFiles = append(referencedFiles, referencedFile{path: reference.Path, info: info})
		}
		return nil
	}
	harnessExpected := map[string]string{
		m6RoleOpenClawAgentResult: "agent.json", m6RoleOpenClawAgentStderr: "agent.stderr",
		m6RoleOpenClawGatewayLog: "gateway.log", m6RoleOpenClawTelemetryIndex: "telemetry.index.json",
	}
	if err := validate("harness", outcomes.Harness.Artifacts, harnessExpected, true); err != nil {
		return err
	}
	evaluationExpected := map[string]string{
		m6RoleVerifierCTRF: "fix-git/evaluation/ctrf.json", m6RoleVerifierReward: "fix-git/evaluation/reward.txt",
		m6RoleVerifierStderr: "fix-git/evaluation/stderr.log", m6RoleVerifierStdout: "fix-git/evaluation/stdout.log",
	}
	return validate("evaluation", outcomes.Evaluation.Artifacts, evaluationExpected, false)
}

func readM6PortableRunResult(root, relative string) (m6PortableRunResult, error) {
	path, err := m6ArtifactPath(root, relative)
	if err != nil {
		return m6PortableRunResult{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return m6PortableRunResult{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > m6MaxRunResultBytes {
		return m6PortableRunResult{}, fmt.Errorf("M6 portable run result type/size = %v/%d", info.Mode().Type(), info.Size())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return m6PortableRunResult{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var result m6PortableRunResult
	if err := decoder.Decode(&result); err != nil {
		return m6PortableRunResult{}, fmt.Errorf("decode M6 portable run result: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return m6PortableRunResult{}, errors.New("M6 portable run result has trailing JSON content")
	}
	return result, nil
}

func successfulM6RunSummary() m6PortableRunSummary {
	return m6PortableRunSummary{
		Tasks: 1, HarnessSucceeded: 1, EvaluationsRun: 1,
		EvaluationsSucceeded: 1,
	}
}

func hashM6Artifact(root, relative string) (m6ArtifactEntry, error) {
	path, err := m6ArtifactPath(root, relative)
	if err != nil {
		return m6ArtifactEntry{}, err
	}
	if err := rejectM6SymlinkPath(root, relative); err != nil {
		return m6ArtifactEntry{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return m6ArtifactEntry{}, fmt.Errorf("inspect M6 artifact %q: %w", relative, err)
	}
	if !before.Mode().IsRegular() {
		return m6ArtifactEntry{}, fmt.Errorf("M6 artifact %q is not regular", relative)
	}
	file, err := os.Open(path)
	if err != nil {
		return m6ArtifactEntry{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil || statErr != nil || closeErr != nil {
		return m6ArtifactEntry{}, errors.Join(copyErr, statErr, closeErr)
	}
	if !os.SameFile(before, after) || size != before.Size() || !after.Mode().IsRegular() {
		return m6ArtifactEntry{}, fmt.Errorf("M6 artifact %q changed while hashing", relative)
	}
	return m6ArtifactEntry{Path: relative, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}, nil
}

func m6ArtifactPath(root, relative string) (string, error) {
	if relative == "" || relative == "." || filepath.IsAbs(relative) || strings.ContainsRune(relative, 0) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))) != relative || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("invalid relative M6 artifact path %q", relative)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootAbs, filepath.FromSlash(relative))
	contained, err := filepath.Rel(rootAbs, path)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("M6 artifact path %q escapes evidence root", relative)
	}
	return path, nil
}

func rejectM6SymlinkPath(root, relative string) error {
	current, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(current)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("M6 artifact root is not a real directory")
	}
	parts := strings.Split(relative, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("M6 artifact path %q traverses a symlink", relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("M6 artifact path %q has non-directory ancestor", relative)
		}
	}
	return nil
}

func testM6Manifest(t *testing.T, root string) m6Manifest {
	t.Helper()
	for _, relative := range append(slices.Clone(m6RequiredFixedArtifacts),
		"harnesses/test/agent.json", "harnesses/test/agent.stderr", "harnesses/test/gateway.log",
		"harnesses/test/telemetry.index.json", "harnesses/test/telemetry/test.trajectory.jsonl",
	) {
		if relative == "m6/run-result.json" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(relative+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outcomes := testM6PortableTaskResult()
	portableRunResult := m6PortableRunResult{
		Name: "test-run", RunID: "run", Tasks: []m6PortableTaskResult{outcomes},
		Summary: successfulM6RunSummary(), Duration: 4 * time.Second,
	}
	if err := writeM6JSONArtifact(root, "m6/run-result.json", portableRunResult); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectM6ArtifactEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	return m6Manifest{
		SchemaVersion: m6SchemaVersion, RunID: "run", TaskID: "fix-git",
		Pins:      m6Pins{TerminalBenchRevision: terminalbench.Revision, OpenClawImage: PinnedImage, TaskImage: m6FixGitTaskImage, Model: "aries-deterministic"},
		Outcomes:  outcomes,
		RunResult: m6ArtifactReference{Role: m6RoleRunResult, Path: "m6/run-result.json"},
		Observer:  m6ObserverEvidence{Status: core.StatusNotEnabled, Samples: []m6ObserverSample{}},
		Verifier: m6VerifierEvidence{
			RewardBytes: "1\n", Cases: slices.Clone(m6ExpectedVerifierCases),
			Sources: slices.Clone(m6ExpectedVerifierSources),
		},
		Artifacts: artifacts,
		Inventory: m6ResourceInventory{Containers: []string{}, Volumes: []string{}, Networks: []string{}},
	}
}

func testM6PortableTaskResult() m6PortableTaskResult {
	return m6PortableTaskResult{
		TaskID: "fix-git",
		Harness: m6PortableHarnessResult{
			Status: core.StatusSucceeded, Duration: time.Second,
			Artifacts: []m6ArtifactReference{
				{Role: m6RoleOpenClawAgentResult, Path: "harnesses/test/agent.json"},
				{Role: m6RoleOpenClawAgentStderr, Path: "harnesses/test/agent.stderr"},
				{Role: m6RoleOpenClawGatewayLog, Path: "harnesses/test/gateway.log"},
				{Role: m6RoleOpenClawTelemetryIndex, Path: "harnesses/test/telemetry.index.json"},
			},
		},
		Isolation: m6PortableIsolationResult{Status: core.StatusConfirmed, HarnessStopped: true, BridgeRevoked: true},
		Evaluation: m6PortableEvaluationResult{
			Status: core.StatusSucceeded, Score: 1, Reward: 1, VerifierStatus: core.StatusSucceeded, Duration: time.Second,
			Artifacts: []m6ArtifactReference{
				{Role: m6RoleVerifierCTRF, Path: "fix-git/evaluation/ctrf.json"},
				{Role: m6RoleVerifierReward, Path: "fix-git/evaluation/reward.txt"},
				{Role: m6RoleVerifierStderr, Path: "fix-git/evaluation/stderr.log"},
				{Role: m6RoleVerifierStdout, Path: "fix-git/evaluation/stdout.log"},
			},
		},
		Observer: m6PortableStatusResult{Status: core.StatusNotEnabled},
		Cleanup:  m6PortableStatusResult{Status: core.StatusSucceeded},
		Duration: 3 * time.Second,
	}
}

func TestM6EvidenceDirectoryIsUniquePrivateAndIgnoredShape(t *testing.T) {
	first, err := newM6EvidenceDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := newM6EvidenceDir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(first)))))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(filepath.ToSlash(first), "/.cache/integration/m6/fix-git-") || !strings.Contains(filepath.ToSlash(second), "/.cache/integration/m6/fix-git-") {
		t.Fatalf("M6 evidence directories = %q and %q", first, second)
	}
	for _, path := range []string{first, second} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("M6 evidence directory %q = %v, %v", path, info, err)
		}
	}
}

func TestM6ManifestExclusiveReadonlyRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "evidence.txt"), []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := testM6Manifest(t, root)
	if err := writeM6Manifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	readBack, err := readM6Manifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(readBack.Artifacts, manifest.Artifacts) || readBack.SchemaVersion != m6SchemaVersion {
		t.Fatalf("manifest readback = %#v", readBack)
	}
	if err := writeM6Manifest(root, manifest); err == nil {
		t.Fatal("exclusive manifest creation overwrote an existing manifest")
	}
}

func TestM6ManifestRejectsInvalidArtifactReferences(t *testing.T) {
	for name, mutate := range map[string]func(*m6Manifest){
		"absolute":  func(manifest *m6Manifest) { manifest.Artifacts[0].Path = "/tmp/evidence" },
		"escape":    func(manifest *m6Manifest) { manifest.Artifacts[0].Path = "../evidence.txt" },
		"self":      func(manifest *m6Manifest) { manifest.Artifacts[0].Path = m6ManifestName },
		"duplicate": func(manifest *m6Manifest) { manifest.Artifacts = append(manifest.Artifacts, manifest.Artifacts[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "evidence.txt"), []byte("evidence\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := testM6Manifest(t, root)
			mutate(&manifest)
			if err := writeM6Manifest(root, manifest); err == nil {
				t.Fatalf("invalid %s artifact reference was accepted", name)
			}
		})
	}
}

func TestM6ManifestRejectsInvalidOutcomeArtifactReferences(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, string, *m6Manifest){
		"absolute": func(_ *testing.T, _ string, manifest *m6Manifest) {
			manifest.Outcomes.Harness.Artifacts[0].Path = "/tmp/agent.json"
		},
		"escape": func(_ *testing.T, _ string, manifest *m6Manifest) {
			manifest.Outcomes.Harness.Artifacts[0].Path = "../agent.json"
		},
		"missing": func(_ *testing.T, _ string, manifest *m6Manifest) {
			manifest.Outcomes.Harness.Artifacts[0].Path = "harnesses/missing/agent.json"
		},
		"unlinked": func(t *testing.T, root string, manifest *m6Manifest) {
			path := filepath.Join(root, "harnesses", "unlinked", "agent.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not in manifest artifact table\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest.Outcomes.Harness.Artifacts[0].Path = "harnesses/unlinked/agent.json"
		},
		"duplicate": func(_ *testing.T, _ string, manifest *m6Manifest) {
			manifest.Outcomes.Harness.Artifacts = append(manifest.Outcomes.Harness.Artifacts, manifest.Outcomes.Harness.Artifacts[0])
		},
		"noncanonical alias": func(_ *testing.T, _ string, manifest *m6Manifest) {
			manifest.Outcomes.Harness.Artifacts[0].Path = "harnesses/test/../test/agent.json"
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := testM6Manifest(t, root)
			mutate(t, root, &manifest)
			if err := writeM6Manifest(root, manifest); err == nil {
				t.Fatalf("invalid %s outcome artifact reference was accepted", name)
			}
		})
	}

	t.Run("filesystem alias", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM6Manifest(t, root)
		stdout := filepath.Join(root, "fix-git", "evaluation", "stdout.log")
		stderr := filepath.Join(root, "fix-git", "evaluation", "stderr.log")
		if err := os.Remove(stderr); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(stdout, stderr); err != nil {
			t.Fatal(err)
		}
		artifacts, err := collectM6ArtifactEntries(root)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts = artifacts
		if err := writeM6Manifest(root, manifest); err == nil {
			t.Fatal("outcome artifact hard-link alias was accepted")
		}
	})
}

func TestNewM6PortableRunResultNormalizesWithoutMutatingRunnerResult(t *testing.T) {
	root := t.TempDir()
	harnessPaths := []string{
		filepath.Join(root, "harnesses", "test", "agent.json"),
		filepath.Join(root, "harnesses", "test", "agent.stderr"),
		filepath.Join(root, "harnesses", "test", "gateway.log"),
		filepath.Join(root, "harnesses", "test", "telemetry.index.json"),
	}
	evaluationPaths := []string{
		filepath.Join(root, "fix-git", "evaluation", "stdout.log"),
		filepath.Join(root, "fix-git", "evaluation", "stderr.log"),
		filepath.Join(root, "fix-git", "evaluation", "ctrf.json"),
		filepath.Join(root, "fix-git", "evaluation", "reward.txt"),
	}
	runnerResult := core.RunResult{
		Name: "test-run", RunID: "run", Duration: 4 * time.Second,
		Tasks: []core.TaskResult{{
			TaskID:     "fix-git",
			Harness:    core.HarnessResult{Status: core.StatusSucceeded, LogPaths: slices.Clone(harnessPaths)},
			Isolation:  core.IsolationResult{Status: core.StatusConfirmed, HarnessStopped: true, BridgeRevoked: true},
			Evaluation: core.Evaluation{Status: core.StatusSucceeded, Score: 1, Reward: 1, VerifierStatus: core.StatusSucceeded, LogPaths: slices.Clone(evaluationPaths)},
			Observer:   core.ObserverResult{Status: core.StatusNotEnabled}, Cleanup: core.CleanupResult{Status: core.StatusSucceeded},
		}},
		Summary: core.RunSummary{Tasks: 1, HarnessSucceeded: 1, EvaluationsRun: 1, EvaluationsSucceeded: 1},
	}
	originalHarnessPaths := slices.Clone(runnerResult.Tasks[0].Harness.LogPaths)
	originalEvaluationPaths := slices.Clone(runnerResult.Tasks[0].Evaluation.LogPaths)
	portable, err := newM6PortableRunResult(root, runnerResult)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runnerResult.Tasks[0].Harness.LogPaths, originalHarnessPaths) || !slices.Equal(runnerResult.Tasks[0].Evaluation.LogPaths, originalEvaluationPaths) {
		t.Fatal("portable conversion mutated the Runner-owned result")
	}
	if len(portable.Tasks) != 1 {
		t.Fatalf("portable tasks = %#v", portable.Tasks)
	}
	for _, reference := range append(slices.Clone(portable.Tasks[0].Harness.Artifacts), portable.Tasks[0].Evaluation.Artifacts...) {
		if filepath.IsAbs(reference.Path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(reference.Path))) != reference.Path {
			t.Fatalf("portable reference is not a clean relative slash path: %#v", reference)
		}
	}
}

func TestM6ManifestRejectsRawRunResultArtifact(t *testing.T) {
	root := t.TempDir()
	manifest := testM6Manifest(t, root)
	raw := core.RunResult{
		Name: "raw-run", RunID: "run",
		Tasks: []core.TaskResult{{
			TaskID: "fix-git",
			Harness: core.HarnessResult{
				Status:   core.StatusSucceeded,
				LogPaths: []string{filepath.Join(root, "harnesses", "test", "agent.json")},
			},
		}},
	}
	content, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "m6", "run-result.json"), append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Artifacts, err = collectM6ArtifactEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeM6Manifest(root, manifest); err == nil {
		t.Fatal("raw core.RunResult with absolute log paths was accepted")
	}
}

func TestM6ManifestRejectsSymlinkAndStaleArtifacts(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "evidence.txt")); err != nil {
			t.Fatal(err)
		}
		manifest := testM6ManifestSkeleton()
		manifest.Artifacts = []m6ArtifactEntry{{Path: "evidence.txt", SHA256: strings.Repeat("0", 64), Size: 9}}
		if err := writeM6Manifest(root, manifest); err == nil {
			t.Fatal("symlink artifact was accepted")
		}
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "evidence.txt"), []byte("evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}
		manifest := testM6ManifestSkeleton()
		manifest.Artifacts = []m6ArtifactEntry{{Path: "linked/evidence.txt", SHA256: strings.Repeat("0", 64), Size: 9}}
		if err := writeM6Manifest(root, manifest); err == nil {
			t.Fatal("symlink artifact ancestor was accepted")
		}
	})

	t.Run("stale", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "evidence.txt")
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := testM6Manifest(t, root)
		if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeM6Manifest(root, manifest); err == nil {
			t.Fatal("stale artifact metadata was accepted")
		}
	})
}

func testM6ManifestSkeleton() m6Manifest {
	return m6Manifest{
		SchemaVersion: m6SchemaVersion, RunID: "run", TaskID: "fix-git",
		Pins:      m6Pins{TerminalBenchRevision: terminalbench.Revision, OpenClawImage: PinnedImage, TaskImage: m6FixGitTaskImage, Model: "aries-deterministic"},
		Outcomes:  testM6PortableTaskResult(),
		RunResult: m6ArtifactReference{Role: m6RoleRunResult, Path: "m6/run-result.json"},
		Observer:  m6ObserverEvidence{Status: core.StatusNotEnabled, Samples: []m6ObserverSample{}},
		Verifier: m6VerifierEvidence{
			RewardBytes: "1\n", Cases: slices.Clone(m6ExpectedVerifierCases),
			Sources: slices.Clone(m6ExpectedVerifierSources),
		},
		Inventory: m6ResourceInventory{Containers: []string{}, Volumes: []string{}, Networks: []string{}},
	}
}

func TestM6ManifestStrictReadRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	content := []byte(`{"schema_version":"aries.m6.oracle.v1","unknown":true}`)
	if err := os.WriteFile(filepath.Join(root, m6ManifestName), content, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readM6Manifest(root); err == nil {
		t.Fatal("manifest with unknown fields was accepted")
	}
}

func TestM6ManifestReadRejectsOversizedAndTrailingContent(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		root := t.TempDir()
		content := bytes.Repeat([]byte("x"), int(m6MaxManifestBytes)+1)
		if err := os.WriteFile(filepath.Join(root, m6ManifestName), content, 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := readM6Manifest(root); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
			t.Fatalf("oversized manifest error = %v", err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		root := t.TempDir()
		manifest := testM6Manifest(t, root)
		content, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, []byte("\n{}\n")...)
		if err := os.WriteFile(filepath.Join(root, m6ManifestName), content, 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := readM6Manifest(root); err == nil || !strings.Contains(err.Error(), "trailing JSON content") {
			t.Fatalf("trailing manifest error = %v", err)
		}
	})
}

func TestWriteM6ArtifactIsExclusiveAndRejectsManifest(t *testing.T) {
	root := t.TempDir()
	if err := writeM6Artifact(root, "nested/evidence.txt", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeM6Artifact(root, "nested/evidence.txt", []byte("two")); err == nil {
		t.Fatal("artifact overwrite was accepted")
	}
	if err := writeM6Artifact(root, m6ManifestName, bytes.Repeat([]byte("x"), 1)); err == nil {
		t.Fatal("generic artifact writer created the manifest")
	}
}
