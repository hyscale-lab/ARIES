//go:build integration

package terminalbench

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedFixGitCheckout(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned Terminal-Bench checkout is absent; run make setup-terminalbench")
		}
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), root); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
	benchmark, err := New(Options{Root: root, TaskIDs: []string{fixGitID}, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != fixGitID || tasks[0].Environment.Image != fixGitImagePin {
		t.Fatalf("Tasks() = %#v", tasks)
	}
}
