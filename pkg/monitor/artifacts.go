package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	artifactDirectoryMode = 0o700
	artifactFileMode      = 0o600
	indexSchemaVersion    = 3
	maxCoverageEntries    = 32
)

// ResourceSample is one bounded resource observation written to resources.jsonl.
type ResourceSample struct {
	Sequence            uint64                   `json:"sequence"`
	Second              uint64                   `json:"second"`
	Time                string                   `json:"time"`
	TaskID              string                   `json:"task_id"`
	Component           string                   `json:"component"`
	RuntimeID           string                   `json:"runtime_id"`
	RuntimeName         string                   `json:"runtime_name"`
	CPUUsageNanoseconds uint64                   `json:"cpu_usage_nanoseconds"`
	CPUPercent          float64                  `json:"cpu_percent"`
	MemoryUsageBytes    uint64                   `json:"memory_usage_bytes"`
	MemoryLimitBytes    uint64                   `json:"memory_limit_bytes"`
	GPU                 *core.GPUResourceReading `json:"gpu,omitempty"`
}

// Index is the bounded summary written beside a task's resource samples.
type Index struct {
	SchemaVersion        int                 `json:"schema_version"`
	RunID                string              `json:"run_id"`
	TaskID               string              `json:"task_id"`
	Status               string              `json:"status"`
	Error                string              `json:"error,omitempty"`
	StartedAt            string              `json:"started_at"`
	StoppedAt            string              `json:"stopped_at"`
	DurationNanoseconds  int64               `json:"duration_nanoseconds"`
	IntervalMilliseconds int64               `json:"interval_milliseconds"`
	SampleCount          uint64              `json:"sample_count"`
	ResourcesFile        string              `json:"resources_file"`
	Components           []ComponentCoverage `json:"components"`
}

// ComponentCoverage records which concrete runtime IDs contributed samples.
type ComponentCoverage struct {
	Component   string `json:"component"`
	RuntimeID   string `json:"runtime_id"`
	RuntimeName string `json:"runtime_name"`
	SampleCount uint64 `json:"sample_count"`
	FirstSecond uint64 `json:"first_second"`
	LastSecond  uint64 `json:"last_second"`
}

type taskArtifact struct {
	taskID        string
	directory     string
	resourcesPath string
	indexPath     string
	resources     *os.File
	sequence      uint64
	bytesWritten  int64
	coverage      map[string]*ComponentCoverage
	closed        bool
}

type artifactOperations struct {
	closeFile func(*os.File) error
	remove    func(string) error
}

func defaultArtifactOperations() artifactOperations {
	return artifactOperations{
		closeFile: func(file *os.File) error { return file.Close() },
		remove:    os.Remove,
	}
}

func (operations artifactOperations) close(file *os.File) error {
	if operations.closeFile == nil {
		return file.Close()
	}
	return operations.closeFile(file)
}

func (operations artifactOperations) removePath(path string) error {
	if operations.remove == nil {
		return os.Remove(path)
	}
	return operations.remove(path)
}

func prepareArtifacts(outputDir string, taskIDs []string, operations artifactOperations) (map[string]*taskArtifact, error) {
	if err := rejectExistingSymlinks(outputDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, artifactDirectoryMode); err != nil {
		return nil, fmt.Errorf("create monitor output directory: %w", err)
	}
	artifacts := make(map[string]*taskArtifact, len(taskIDs))
	rollback := func(cause error) (map[string]*taskArtifact, error) {
		cleanupErr := removePartialArtifacts(artifacts, operations)
		if cleanupErr != nil {
			cause = errors.Join(cause, fmt.Errorf("roll back partial monitor artifact preparation: %w", cleanupErr))
		}
		return nil, cause
	}
	for _, taskID := range taskIDs {
		taskRoot := filepath.Join(outputDir, taskID)
		if err := os.MkdirAll(taskRoot, artifactDirectoryMode); err != nil {
			return rollback(fmt.Errorf("create monitor task root for %s: %w", taskID, err))
		}
		if err := validatePrivateDirectory(taskRoot); err != nil {
			return rollback(err)
		}
		directory := filepath.Join(taskRoot, "monitor")
		if err := os.Mkdir(directory, artifactDirectoryMode); err != nil {
			return rollback(fmt.Errorf("create monitor task directory for %s: %w", taskID, err))
		}
		artifact := &taskArtifact{
			taskID:        taskID,
			directory:     directory,
			resourcesPath: filepath.Join(directory, "resources.jsonl"),
			indexPath:     filepath.Join(directory, "index.json"),
			coverage:      make(map[string]*ComponentCoverage),
		}
		artifacts[taskID] = artifact
		file, err := os.OpenFile(artifact.resourcesPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, artifactFileMode)
		if err != nil {
			return rollback(fmt.Errorf("create monitor sample file for %s: %w", taskID, err))
		}
		artifact.resources = file
	}
	return artifacts, nil
}

func (artifact *taskArtifact) appendSample(sample ResourceSample, maxSamples int, maxFileBytes int64) error {
	if artifact.closed || artifact.resources == nil {
		return errors.New("monitor sample file is closed")
	}
	if artifact.sequence >= uint64(maxSamples) {
		return fmt.Errorf("monitor sample count exceeds %d", maxSamples)
	}
	coverageKey := sample.Component + "\x00" + sample.RuntimeID
	coverage, ok := artifact.coverage[coverageKey]
	if !ok && len(artifact.coverage) >= maxCoverageEntries {
		return fmt.Errorf("monitor component coverage exceeds %d entries", maxCoverageEntries)
	}
	sample.Sequence = artifact.sequence
	content, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("encode monitor sample: %w", err)
	}
	content = append(content, '\n')
	if artifact.bytesWritten+int64(len(content)) > maxFileBytes {
		return fmt.Errorf("monitor sample file exceeds %d bytes", maxFileBytes)
	}
	if err := writeAll(artifact.resources, content); err != nil {
		return fmt.Errorf("write monitor sample: %w", err)
	}
	artifact.sequence++
	artifact.bytesWritten += int64(len(content))
	if !ok {
		coverage = &ComponentCoverage{
			Component:   sample.Component,
			RuntimeID:   sample.RuntimeID,
			RuntimeName: sample.RuntimeName,
			FirstSecond: sample.Second,
		}
		artifact.coverage[coverageKey] = coverage
	}
	coverage.SampleCount++
	coverage.LastSecond = sample.Second
	return nil
}

func (artifact *taskArtifact) closeResources(operations artifactOperations) error {
	if artifact.closed {
		return nil
	}
	if artifact.resources == nil {
		artifact.closed = true
		return nil
	}
	if err := artifact.resources.Sync(); err != nil {
		return fmt.Errorf("sync monitor samples for %s: %w", artifact.taskID, err)
	}
	file := artifact.resources
	if err := operations.close(file); err != nil {
		artifact.resources = nil
		artifact.closed = true
		return fmt.Errorf("close monitor samples for %s: %w", artifact.taskID, err)
	}
	artifact.resources = nil
	artifact.closed = true
	return nil
}

func (artifact *taskArtifact) componentCoverage() []ComponentCoverage {
	coverage := make([]ComponentCoverage, 0, len(artifact.coverage))
	for _, item := range artifact.coverage {
		coverage = append(coverage, *item)
	}
	sort.Slice(coverage, func(i, j int) bool {
		if coverage[i].Component != coverage[j].Component {
			return coverage[i].Component < coverage[j].Component
		}
		return coverage[i].RuntimeID < coverage[j].RuntimeID
	})
	return coverage
}

func writeIndexExact(path string, index Index) error {
	content, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("encode monitor index: %w", err)
	}
	content = append(content, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, artifactFileMode)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := readPrivateFile(path, int64(len(content))+1)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, content) {
			return errors.New("existing monitor index differs from the retry output")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create monitor index: %w", err)
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	if err := writeAll(file, content); err != nil {
		return fmt.Errorf("write monitor index: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync monitor index: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close monitor index: %w", err)
	}
	succeeded = true
	return nil
}

func readPrivateFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect existing monitor index: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != artifactFileMode {
		return nil, errors.New("existing monitor index is not a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open existing monitor index: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read existing monitor index: %w", err)
	}
	if int64(len(content)) > maxBytes {
		return nil, errors.New("existing monitor index exceeds its expected bound")
	}
	return content, nil
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) != 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect monitor artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != artifactDirectoryMode {
		return fmt.Errorf("monitor artifact directory %q is not a private directory", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("monitor artifact directory %q is not owned by the current user", path)
	}
	return nil
}

func rejectExistingSymlinks(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect monitor output path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("monitor output path contains symlink %q", current)
		}
	}
	return nil
}

func removePartialArtifacts(artifacts map[string]*taskArtifact, operations artifactOperations) error {
	var failures []error
	for _, artifact := range artifacts {
		if artifact.resources != nil {
			if err := operations.close(artifact.resources); err != nil {
				failures = append(failures, err)
			}
		}
		if err := operations.removePath(artifact.resourcesPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		if err := operations.removePath(artifact.indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		if err := operations.removePath(artifact.directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func formatArtifactTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
