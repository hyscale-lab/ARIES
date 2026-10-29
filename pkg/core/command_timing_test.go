package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestCommandTimingsPreserveNestedStagesAndDetachedSnapshots(t *testing.T) {
	ctx, timings := WithCommandTimings(context.Background())
	coreStart := timings.started
	finishOuter := TimeCommandStage(ctx, "restore")
	finishInner := TimeCommandStage(ctx, "restore.mounts")
	time.Sleep(time.Millisecond)
	finishInner()
	finishInner()
	MarkCommandTiming(ctx, "command.completed")
	first := timings.Snapshot()
	point := first.Milestones["command.completed"]
	time.Sleep(time.Millisecond)
	MarkCommandTiming(ctx, "command.completed")
	finishOuter()
	final := timings.Snapshot()
	if len(first.Stages) != 1 || len(final.Stages) != 2 {
		t.Fatalf("stages duplicated or snapshot mutated: first=%v final=%v", first.Stages, final.Stages)
	}
	if final.Stages[0].Name != "restore" || final.Stages[1].Name != "restore.mounts" || final.Stages[0].DurationMS < final.Stages[1].DurationMS {
		t.Fatalf("nested stages lost: %#v", final.Stages)
	}
	if point != final.Milestones["command.completed"] {
		t.Fatal("first milestone overwritten")
	}
	if point.At.Location() != time.UTC || final.ReceivedAt.Location() != time.UTC || !final.ReceivedAt.Equal(coreStart) {
		t.Fatal("timing timestamp is not UTC observation")
	}
	first.Milestones["command.completed"] = CommandTimingPoint{}
	first.Stages[0].Name = "mutated"
	again := timings.Snapshot()
	if again.Milestones["command.completed"] != point || again.Stages[1].Name != "restore.mounts" {
		t.Fatal("snapshot shares mutable data")
	}
	if _, err := json.Marshal(final); err != nil {
		t.Fatal(err)
	}
}

func TestCommandTimingsConcurrentObservations(t *testing.T) {
	ctx, timings := WithCommandTimings(context.Background())
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			finish := TimeCommandStage(ctx, "stream.connect")
			MarkCommandTiming(ctx, "command.started")
			finish()
			_ = timings.Snapshot()
		}()
	}
	workers.Wait()
	snapshot := timings.Snapshot()
	if len(snapshot.Stages) != 32 || len(snapshot.Milestones) != 1 {
		t.Fatalf("observations lost: %#v", snapshot)
	}
	for _, stage := range snapshot.Stages {
		if stage.OffsetMS < 0 || stage.DurationMS < 0 {
			t.Fatalf("negative stage: %#v", stage)
		}
	}
	TimeCommandStage(context.Background(), "unobserved")()
	MarkCommandTiming(context.Background(), "unobserved")
}
