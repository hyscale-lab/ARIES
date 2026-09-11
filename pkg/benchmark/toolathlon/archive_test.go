package toolathlon

import (
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

func TestWriteProjectArchiveFailsForMissingTask(t *testing.T) {
	root := writeFixture(t)
	if err := writeProjectArchive(root, "missing-task", filepath.Join(t.TempDir(), "project.tar")); err == nil {
		t.Fatal("expected a missing task directory to be an error")
	}
}
