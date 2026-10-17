package swebenchpro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparedCatalogLoadsDatasetOnceAndRetainsPrivateDetails(t *testing.T) {
	rows := make([]datasetRow, publicTaskCount)
	for i := range rows {
		rows[i] = fixtureRow()
		rows[i].InstanceID = fmt.Sprintf("task-%03d", i)
	}
	dataset, evaluator := writeDatasetFixture(t, rows)
	for _, row := range rows {
		writeTestFile(t, filepath.Join(evaluator, runScriptsDirectory, row.InstanceID, runScriptName), "#!/bin/bash\n")
		writeTestFile(t, filepath.Join(evaluator, runScriptsDirectory, row.InstanceID, parserFileName), "# parser\n")
	}
	root := t.TempDir()
	if err := os.Rename(dataset, filepath.Join(root, "dataset")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(evaluator, filepath.Join(root, "evaluator")); err != nil {
		t.Fatal(err)
	}
	catalog, err := New(Options{
		Root: root, TaskIDs: []string{rows[0].InstanceID, rows[1].InstanceID}, OutputDir: t.TempDir(),
		DatasetRevision:   commitEvaluationFixture(t, filepath.Join(root, "dataset")),
		EvaluatorRevision: commitEvaluationFixture(t, filepath.Join(root, "evaluator")),
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := catalog.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "dataset", datasetParquetPath)); err != nil {
		t.Fatal(err)
	}
	for _, logical := range tasks {
		for i := range 2 {
			id := fmt.Sprintf("%s-%03d", logical.ID, i+1)
			role, err := catalog.ForTask(logical.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			owner := role.(*Benchmark)
			selected, err := owner.Tasks(context.Background())
			if err != nil || len(selected) != 1 || selected[0].ID != id {
				t.Fatalf("tasks = %v, %v", selected, err)
			}
			if owner.details[id].testPatch != fixtureTestPatch || len(owner.details[id].failToPass) == 0 {
				t.Fatal("private test inputs lost")
			}
			encoded, err := json.Marshal(selected[0])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), fixtureTestPatch) || strings.Contains(string(encoded), fixtureGoldPatch) {
				t.Fatal("private inputs exposed in task")
			}
		}
	}
}
