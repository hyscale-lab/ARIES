package toolathlon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWriteProjectArchiveCopiesWhatTheRunnerCopies(t *testing.T) {
	root := writeFixture(t)
	archive := filepath.Join(t.TempDir(), "project.tar")
	if err := writeProjectArchive(root, "canvas-list-test", archive); err != nil {
		t.Fatal(err)
	}
	members, err := archiveMemberNames(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"configs/global_configs.py",
		"configs/token_key_session.py",
		"configs/mcp_servers/canvas.yaml",
		"scripts/formal_run_v0.json",
		"scripts/decoupled/container_preprocess.py",
		"utils/helper.py",
		"main.py",
		"global_preparation/check_installation.py",
		"tasks/finalpool/canvas-list-test/task_config.json",
		"tasks/finalpool/canvas-list-test/docs/task.md",
		"tasks/finalpool/canvas-list-test/evaluation/main.py",
		"tasks/finalpool/canvas-list-test/groundtruth_workspace/expected.csv",
	} {
		if !slices.Contains(members, want) {
			t.Fatalf("archive lacks %s; members: %v", want, members)
		}
	}
	for _, member := range members {
		switch {
		case strings.Contains(member, "__pycache__"), strings.Contains(member, ".mcp-auth"):
			t.Fatalf("archive carries ignored content: %s", member)
		case strings.HasPrefix(member, "local_binary/"), strings.HasPrefix(member, "deployment/"):
			t.Fatalf("archive carries an entry the adapter does not need: %s", member)
		case strings.HasPrefix(member, "global_preparation/deploy_containers.sh"):
			t.Fatalf("archive carries more of global_preparation than the one file: %s", member)
		case strings.HasPrefix(member, "tasks/finalpool/excel-only/"):
			t.Fatalf("archive carries another task: %s", member)
		}
	}
}

// A symlink is followed inside the sandbox, so its target is judged where it
// resolves from the link's own directory, not by how it is spelled.
func TestWriteProjectArchiveJudgesSymlinksWhereTheyResolve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		escapes bool
	}{
		{name: "sibling", target: "task_config.json"},
		{name: "up and back down inside the checkout", target: "../canvas-list-test/task_config.json"},
		{name: "dotdot after a segment, still inside", target: "docs/../task_config.json"},
		{name: "plain parent escape", target: "../../../../outside", escapes: true},
		{name: "escape hidden behind a segment", target: "docs/../../../../../outside", escapes: true},
		{name: "absolute", target: "/etc/passwd", escapes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeFixture(t)
			link := filepath.Join(root, "tasks", "finalpool", "canvas-list-test", "link")
			if err := os.Symlink(filepath.FromSlash(tc.target), link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := writeProjectArchive(root, "canvas-list-test", filepath.Join(t.TempDir(), "project.tar"))
			if tc.escapes {
				if err == nil || !strings.Contains(err.Error(), "escapes the checkout") {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestWriteProjectArchiveFailsForMissingTask(t *testing.T) {
	root := writeFixture(t)
	if err := writeProjectArchive(root, "missing-task", filepath.Join(t.TempDir(), "project.tar")); err == nil {
		t.Fatal("expected a missing task directory to be an error")
	}
}
