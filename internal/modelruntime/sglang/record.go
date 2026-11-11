package sglang

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/internal/procstat"
)

// processRecordName is the file beside the managed server's logs that names its
// process group. The server runs in a group of its own and outlives an ARIES
// that is killed, so a later `aries cleanup` finds and stops it through this.
const processRecordName = "process.json"

// processRecord identifies the server's process group by its leader's PID and
// start time, which tells the leader apart from a later process that reuses
// its PID.
type processRecord struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// writeProcessRecord records the group that pid leads. A server that has
// already exited leaves nothing to stop and is not recorded.
func writeProcessRecord(artifactDir string, pid int) error {
	start, alive, err := procstat.StartTime(pid)
	if err != nil || !alive {
		return err
	}
	content, err := json.Marshal(processRecord{PID: pid, Start: start})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(artifactDir, "."+processRecordName+".*")
	if err != nil {
		return err
	}
	_, writeErr := temporary.Write(append(content, '\n'))
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	if err := os.Rename(temporary.Name(), filepath.Join(artifactDir, processRecordName)); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	return nil
}

func readProcessRecord(outputDir string) (processRecord, error) {
	raw, err := os.ReadFile(filepath.Join(outputDir, "sglang", processRecordName))
	if err != nil {
		return processRecord{}, err
	}
	var record processRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return processRecord{}, fmt.Errorf("decode managed SGLang %s: %w", processRecordName, err)
	}
	if record.PID <= 1 || record.Start == 0 {
		return processRecord{}, fmt.Errorf("managed SGLang %s is incomplete", processRecordName)
	}
	return record, nil
}

// groupAlive reports whether the recorded process group still has a member.
// The kernel does not reuse a PID while it names a live process group, so a
// group whose leader has exited is still the server's; a live leader with
// another start time is a later process that reused the PID.
func (record processRecord) groupAlive() (bool, error) {
	if err := syscall.Kill(-record.PID, 0); errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("probe managed SGLang process group: %w", err)
	}
	start, alive, err := procstat.StartTime(record.PID)
	if err != nil {
		return false, err
	}
	return !alive || start == record.Start, nil
}

// RecordedServer reports whether the managed SGLang server that a run's
// directory records is still running, and its process group ID. A run without
// a record, as one that used no managed server, has none.
func RecordedServer(outputDir string) (int, bool, error) {
	record, err := readProcessRecord(outputDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	alive, err := record.groupAlive()
	return record.PID, alive, err
}

// StopRecordedServer ends the recorded server's process group as Stop does:
// SIGTERM, SIGKILL once grace has passed, and confirmation that the group is
// gone. A group that is already gone, or no record, is left alone; it is safe
// to repeat.
func StopRecordedServer(ctx context.Context, outputDir string, grace time.Duration) error {
	record, err := readProcessRecord(outputDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, step := range []struct {
		signal syscall.Signal
		wait   time.Duration
	}{{syscall.SIGTERM, grace}, {syscall.SIGKILL, forcedProcessWait}} {
		if alive, err := record.groupAlive(); err != nil || !alive {
			return err
		}
		if err := syscall.Kill(-record.PID, step.signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal managed SGLang process group: %w", err)
		}
		deadline := time.Now().Add(step.wait)
		for time.Now().Before(deadline) {
			if alive, err := record.groupAlive(); err != nil || !alive {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return errors.New("managed SGLang process group remains after SIGKILL")
}
