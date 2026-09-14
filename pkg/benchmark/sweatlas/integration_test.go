//go:build integration

package sweatlas

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

func TestPinnedQATaskCheckout(t *testing.T) {
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned SWE-Atlas checkout is absent; run make setup")
		}
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), root, versions.SWEAtlas.Revision); err != nil {
		t.Fatal(err)
	}
	benchmark, err := New(Options{
		Root: root, TaskIDs: []string{qaTaskID}, OutputDir: t.TempDir(),
		Revision:     versions.SWEAtlas.Revision,
		Judge:        core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
		APIKeyLookup: func(string) ([]byte, bool) { return []byte("fake-key"), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != qaTaskID {
		t.Fatalf("Tasks() = %#v", tasks)
	}
}
