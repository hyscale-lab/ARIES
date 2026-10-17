package toolathlon

import (
	"context"
	"os"
	"testing"
)

func TestPreparedOccurrencesKeepRuntimeVerifierStateSeparate(t *testing.T) {
	gatewayReadyDelay, forwarderReadyDelay = 0, 0
	firstSandbox := newFlowSandbox(t, "canvas-list-test", true)
	catalog, task := preparedBenchmark(t, firstSandbox)
	firstRole, err := catalog.ForTask(task.ID, task.ID+"-001")
	if err != nil {
		t.Fatal(err)
	}
	secondRole, err := catalog.ForTask(task.ID, task.ID+"-002")
	if err != nil {
		t.Fatal(err)
	}
	first, second := firstRole.(*Benchmark), secondRole.(*Benchmark)
	firstTasks, err := first.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTask := firstTasks[0]
	firstTask.Environment.Env["TZ"] = "changed"
	firstTask.Environment.Services.MCP[0].Port = 1
	secondTasks, err := second.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondTask := secondTasks[0]
	if secondTask.Environment.Env["TZ"] != "UTC" || secondTask.Environment.Services.MCP[0].Port == 1 {
		t.Fatal("public environment aliases another occurrence")
	}
	if err := first.PrepareSandbox(context.Background(), firstTask, firstSandbox); err != nil {
		t.Fatal(err)
	}
	if len(first.details[firstTask.ID].bundle) == 0 || len(first.details[firstTask.ID].stashed) == 0 {
		t.Fatal("first occurrence lost its prepared verifier")
	}
	if second.details[secondTask.ID].bundle != nil || second.details[secondTask.ID].stashed != nil || catalog.details[task.ID].bundle != nil {
		t.Fatal("first occurrence changed another occurrence's private verifier state")
	}
	secondSandbox := newFlowSandbox(t, task.ID, true)
	if _, err := second.Evaluate(context.Background(), secondTask, secondSandbox, nil); err == nil {
		t.Fatal("unprepared occurrence used another occurrence's verifier")
	}
	// Task loading reuses the snapshot and never overwrites an occurrence's stash.
	if err := os.RemoveAll(catalog.root); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(first.details[firstTask.ID].bundle) == 0 {
		t.Fatal("task reload discarded the verifier bundle")
	}
}
