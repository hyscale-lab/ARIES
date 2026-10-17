package deepresearchbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestPreparedCatalogForksConcurrentOccurrencesWithoutReload(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	catalog, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	catalog.race = &stubRace{}
	tasks, err := catalog.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logicalID := tasks[0].ID
	if _, err := catalog.ForTask(logicalID, "../invalid"); err == nil {
		t.Fatal("unsafe occurrence accepted")
	}
	if _, err := catalog.ForTask("missing", "missing-001"); err == nil {
		t.Fatal("unloaded task accepted")
	}
	// A second source read or revision check during occurrence creation would fail.
	// Evaluation still performs its own pin check before touching private files.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			id := fmt.Sprintf("%s-%03d", logicalID, i+1)
			occurrence, err := catalog.ForTask(logicalID, id)
			if err != nil {
				t.Error(err)
				return
			}
			owner := occurrence.(*Benchmark)
			selected, err := owner.Tasks(context.Background())
			if err != nil || len(selected) != 1 || selected[0].ID != id {
				t.Errorf("occurrence tasks = %v, %v", selected, err)
				return
			}
			input := owner.judgeInputs[owner.numericIDs[id]]
			if input.prompt == "" || input.reference != "reference report" || len(input.criteria.Criterions.Insight) == 0 {
				t.Error("private judge inputs lost")
			}
			encoded, err := json.Marshal(selected[0])
			if err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(string(encoded), "reference report") {
				t.Error("public task leaked verifier material")
			}
		}()
	}
	group.Wait()
}
