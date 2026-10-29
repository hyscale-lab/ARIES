package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

// CommandTiming records one process's observations. Offsets and durations use
// its monotonic clock; UTC timestamps allow correlation with other logs but do
// not establish remote receipt or remove clock skew between hosts.
type CommandTiming struct {
	ReceivedAt time.Time                     `json:"received_at"`
	Stages     []CommandTimingStage          `json:"stages"`
	Milestones map[string]CommandTimingPoint `json:"milestones"`
}

type CommandTimingStage struct {
	Name       string  `json:"name"`
	OffsetMS   float64 `json:"offset_ms"`
	DurationMS float64 `json:"duration_ms"`
}

type CommandTimingPoint struct {
	At       time.Time `json:"at"`
	OffsetMS float64   `json:"offset_ms"`
}

// CommandTimings is shared only by the execution of one borrowed command.
// Providers append their own concrete stages without depending on the bridge.
type CommandTimings struct {
	mu         sync.Mutex
	started    time.Time
	stages     []CommandTimingStage
	milestones map[string]CommandTimingPoint
}

type commandTimingKey struct{}

func WithCommandTimings(ctx context.Context) (context.Context, *CommandTimings) {
	timings := &CommandTimings{started: time.Now(), stages: make([]CommandTimingStage, 0), milestones: make(map[string]CommandTimingPoint)}
	return context.WithValue(ctx, commandTimingKey{}, timings), timings
}

// TimeCommandStage returns an idempotent completion function. It is a no-op
// when the caller has not requested command timing collection.
func TimeCommandStage(ctx context.Context, name string) func() {
	timings, _ := ctx.Value(commandTimingKey{}).(*CommandTimings)
	if timings == nil {
		return func() {}
	}
	started := time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			ended := time.Now()
			timings.mu.Lock()
			timings.stages = append(timings.stages, CommandTimingStage{Name: name, OffsetMS: milliseconds(started.Sub(timings.started)), DurationMS: milliseconds(ended.Sub(started))})
			timings.mu.Unlock()
		})
	}
}

// MarkCommandTiming records a boundary only once, preserving the first
// observation if multiple execution paths report the same milestone.
func MarkCommandTiming(ctx context.Context, name string) {
	timings, _ := ctx.Value(commandTimingKey{}).(*CommandTimings)
	if timings == nil {
		return
	}
	now := time.Now()
	timings.mu.Lock()
	defer timings.mu.Unlock()
	if _, ok := timings.milestones[name]; !ok {
		timings.milestones[name] = CommandTimingPoint{At: now.UTC(), OffsetMS: milliseconds(now.Sub(timings.started))}
	}
}

func (timings *CommandTimings) Snapshot() CommandTiming {
	timings.mu.Lock()
	defer timings.mu.Unlock()
	stages := append(make([]CommandTimingStage, 0, len(timings.stages)), timings.stages...)
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].OffsetMS < stages[j].OffsetMS })
	milestones := make(map[string]CommandTimingPoint, len(timings.milestones))
	for name, point := range timings.milestones {
		milestones[name] = point
	}
	return CommandTiming{ReceivedAt: timings.started.UTC(), Stages: stages, Milestones: milestones}
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
