package replay

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// The bridge audit writer bounds both logs to 256 MiB in total.
const (
	maxTimingAuditBytes   = 256 << 20
	privateTimingFileMode = 0o600
)

// ReadTiming compares finalized bridge starts with one validated source plan.
func ReadTiming(path string, plan Plan) core.ReplayTiming {
	unavailable := func(reason string) core.ReplayTiming {
		return core.ReplayTiming{Status: "unavailable", Reason: reason}
	}
	if path == "" {
		return unavailable("replay bridge audit is missing")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return unavailable("replay bridge audit is unavailable")
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != privateTimingFileMode || info.Size() < 0 || info.Size() > maxTimingAuditBytes {
		return unavailable("replay bridge audit is not a bounded private file")
	}
	file, err := root.Open(name)
	if err != nil {
		return unavailable("replay bridge audit is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != info.Mode().Perm() || opened.Size() != info.Size() {
		return unavailable("replay bridge audit changed before reading")
	}
	starts := make(map[uint64]time.Time, len(plan.Calls))
	var first time.Time
	decoder := json.NewDecoder(io.LimitReader(file, maxTimingAuditBytes+1))
	for {
		var record struct {
			SourceSequence *uint64 `json:"replay_source_sequence"`
			StartedAt      string  `json:"started_at"`
		}
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			return unavailable("replay bridge audit has an invalid record")
		}
		if record.SourceSequence == nil || *record.SourceSequence == 0 {
			return unavailable("replay bridge audit has missing source sequence")
		}
		if _, exists := starts[*record.SourceSequence]; exists {
			return unavailable("replay bridge audit has duplicate source sequence")
		}
		started, err := time.Parse(time.RFC3339Nano, record.StartedAt)
		if err != nil {
			return unavailable("replay bridge audit has an invalid start time")
		}
		starts[*record.SourceSequence] = started
		if first.IsZero() || started.Before(first) {
			first = started
		}
		if len(starts) > len(plan.Calls) {
			return unavailable("replay bridge audit has extra calls")
		}
	}
	closed, err := file.Stat()
	if err != nil || !os.SameFile(info, closed) || closed.Mode().Perm() != info.Mode().Perm() || closed.Size() != info.Size() {
		return unavailable("replay bridge audit changed while reading")
	}
	if len(starts) != len(plan.Calls) {
		return unavailable("replay bridge audit has missing calls")
	}
	timing := core.ReplayTiming{Status: "available"}
	for _, call := range plan.Calls {
		started, exists := starts[call.Sequence]
		if !exists || call.Sequence == 0 || call.StartOffset < 0 {
			return unavailable("replay bridge audit has an unknown source sequence")
		}
		offset := started.Sub(first)
		if !first.Add(offset).Equal(started) {
			return unavailable("replay bridge audit start offset is not representable")
		}
		timing.Calls = append(timing.Calls, core.ReplayCallTiming{SourceSequence: call.Sequence, DriftNS: (offset - call.StartOffset).Nanoseconds()})
	}
	return timing
}
