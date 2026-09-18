package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

// Arrival is one task's scheduled start, relative to the run start.
type Arrival struct {
	TaskID string
	At     time.Duration
}

type arrivalTrace struct {
	TraceID        string  `json:"trace_id"`
	BaseRatePerMin float64 `json:"base_rate_per_min"`
	Arrivals       []struct {
		T    float64 `json:"t"`
		Traj string  `json:"traj"`
	} `json:"arrivals"`
}

// LoadArrivals reads a context-gc arrival trace and returns one start time per
// entry of taskIDs, in start order. The k-th occurrence of a task in taskIDs
// takes the k-th arrival of that task in the trace, and every offset is
// scaled from the trace's base rate to ratePerMin: at rate r a Poisson stream
// recorded at base rate b keeps its order with every time multiplied by b/r.
// A task with no arrival left in the trace is an error, never a silent skip.
func LoadArrivals(path string, ratePerMin float64, taskIDs []string) ([]Arrival, error) {
	if !(ratePerMin > 0) || math.IsInf(ratePerMin, 0) {
		return nil, errors.New("arrival rate must be finite and positive")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read arrivals file: %w", err)
	}
	var trace arrivalTrace
	if err := json.Unmarshal(raw, &trace); err != nil {
		return nil, fmt.Errorf("parse arrivals file %q: %w", path, err)
	}
	if !(trace.BaseRatePerMin > 0) {
		return nil, fmt.Errorf("arrivals file %q: base_rate_per_min must be positive", path)
	}
	if len(trace.Arrivals) == 0 {
		return nil, fmt.Errorf("arrivals file %q has no arrivals", path)
	}
	scale := trace.BaseRatePerMin / ratePerMin
	byTask := make(map[string][]float64)
	for _, entry := range trace.Arrivals {
		byTask[entry.Traj] = append(byTask[entry.Traj], entry.T)
	}
	used := make(map[string]int)
	arrivals := make([]Arrival, 0, len(taskIDs))
	for _, id := range taskIDs {
		times := byTask[id]
		k := used[id]
		if k >= len(times) {
			return nil, fmt.Errorf("arrivals file %q has %d arrival(s) for task %q; the profile lists it %d time(s)", path, len(times), id, k+1)
		}
		used[id] = k + 1
		seconds := times[k] * scale
		if !(seconds >= 0) || math.IsInf(seconds, 0) || seconds*float64(time.Second) >= math.Exp2(63) {
			return nil, fmt.Errorf("arrivals file %q: offset %g for task %q is out of range", path, seconds, id)
		}
		arrivals = append(arrivals, Arrival{TaskID: id, At: time.Duration(seconds * float64(time.Second))})
	}
	sort.SliceStable(arrivals, func(i, j int) bool { return arrivals[i].At < arrivals[j].At })
	return arrivals, nil
}
