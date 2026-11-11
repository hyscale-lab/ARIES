package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/procstat"
)

// runnerRecordName is the file in a run's directory that names the process
// running the run, so that a later `aries cleanup` can refuse while it lives.
const runnerRecordName = "runner.json"

// runnerRecord identifies a Runner process. Start is the process's start time
// in clock ticks after boot, which tells it apart from a later process that
// reuses its PID.
type runnerRecord struct {
	Host  string `json:"host"`
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// writeRunnerRecord records this process as the run's Runner. The file is
// written beside its final name and renamed into place, so a reader never sees
// part of it.
func writeRunnerRecord(outputRoot string, hostname func() (string, error), processStart func(int) (uint64, bool, error)) error {
	host, err := hostname()
	if err != nil {
		return fmt.Errorf("read this host's name: %w", err)
	}
	pid := os.Getpid()
	start, alive, err := processStart(pid)
	if err != nil {
		return fmt.Errorf("read this process's start time: %w", err)
	}
	if !alive {
		return errors.New("read this process's start time: /proc has no entry for it")
	}
	content, err := json.Marshal(runnerRecord{Host: host, PID: pid, Start: start})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(outputRoot, "."+runnerRecordName+".*")
	if err != nil {
		return err
	}
	_, writeErr := temporary.Write(append(content, '\n'))
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	if err := os.Rename(temporary.Name(), filepath.Join(outputRoot, runnerRecordName)); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	return nil
}

// readRunnerRecord reads a run's runner record. A missing one wraps
// os.ErrNotExist.
func readRunnerRecord(outputRoot string) (runnerRecord, error) {
	raw, err := os.ReadFile(filepath.Join(outputRoot, runnerRecordName))
	if err != nil {
		return runnerRecord{}, err
	}
	var record runnerRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return runnerRecord{}, fmt.Errorf("decode %s: %w", runnerRecordName, err)
	}
	if record.Host == "" || record.PID <= 0 || record.Start == 0 {
		return runnerRecord{}, fmt.Errorf("%s is incomplete", runnerRecordName)
	}
	return record, nil
}

func (dependencies Dependencies) hostname() func() (string, error) {
	if dependencies.Hostname != nil {
		return dependencies.Hostname
	}
	return os.Hostname
}

func (dependencies Dependencies) processStart() func(int) (uint64, bool, error) {
	if dependencies.ProcessStart != nil {
		return dependencies.ProcessStart
	}
	return procstat.StartTime
}
