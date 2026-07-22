//go:build integration

package terminalbench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestPinnedFixGitCheckout(t *testing.T) {
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned Terminal-Bench checkout is absent; run make setup")
		}
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), root, versions.TerminalBench2.Revision); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), root, versions.TerminalBench2.RepositoryURL, versions.TerminalBench2.Revision); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
	benchmark, err := New(Options{
		Root: root, TaskIDs: []string{fixGitID}, OutputDir: t.TempDir(),
		Revision: versions.TerminalBench2.Revision, FixGitImage: versions.TerminalBench2.FixGitImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != fixGitID || tasks[0].Environment.Image != versions.TerminalBench2.FixGitImage {
		t.Fatalf("Tasks() = %#v", tasks)
	}
}
