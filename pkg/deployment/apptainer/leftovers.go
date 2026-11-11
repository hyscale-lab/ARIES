package apptainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunInstances names the instances whose records in stateDir carry runID,
// running or not. Manager.Stop removes each, and refuses an instance that is
// no longer the one its record describes.
func RunInstances(stateDir, runID string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Apptainer runtimes: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stateDir, entry.Name(), recordFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read Apptainer runtime record %s: %w", entry.Name(), err)
		}
		var current record
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("decode Apptainer runtime record %s: %w", entry.Name(), err)
		}
		if current.Name == entry.Name() && current.Labels["aries.managed"] == "true" && current.Labels["aries.run"] == runID {
			names = append(names, current.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// RunProcesses names the process runtimes, the run's bridge, whose records in
// stateDir carry runID. ProcessRuntime.Stop ends each one's process group,
// which also ends the run's network namespace, and removes its files.
func RunProcesses(stateDir, runID string) ([]string, error) {
	paths, err := processRecordPaths(stateDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read process runtime record: %w", err)
		}
		var current processFile
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("decode process runtime record %s: %w", filepath.Base(path), err)
		}
		if current.Labels["aries.managed"] == "true" && current.Labels["aries.run"] == runID && current.Name != "" {
			names = append(names, current.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// processRecordPaths lists the process runtime records directly in stateDir.
// The directory is read as a literal path: as a glob pattern, a state_dir with
// a character such as '[' would match nothing and hide every record.
func processRecordPaths(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list process runtime records: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), processRecordSuffix) {
			paths = append(paths, filepath.Join(stateDir, entry.Name()))
		}
	}
	return paths, nil
}
