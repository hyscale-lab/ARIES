package openclaw

import (
	"github.com/hyscale-lab/aries/pkg/core"
	"testing"
)

func TestOperationClassUsesOnlyKnownOpenClawLabels(t *testing.T) {
	for name, test := range map[string]struct {
		command core.Command
		want    string
	}{
		"exact upload": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}},
			want:    "workspace_upload",
		},
		"upload label on other script": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", "true", directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}},
			want:    "exec",
		},
		"upload near target": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualWorkspace, virtualRuntimeRoot}},
			want:    "exec",
		},
	} {
		if got := operationClass(test.command); got != test.want {
			t.Fatalf("operationClass(%q) = %q, want %q", name, got, test.want)
		}
	}
}

func TestReplayDisplayCommandOmitsDuplicatedUploadScript(t *testing.T) {
	execCommand := core.Command{Path: remoteShell, Args: []string{"-c", "git status"}}
	if got := replayDisplayCommand(execCommand); got != "git status" {
		t.Fatalf("exec display command = %q", got)
	}
	uploadCommand := core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}}
	if got := replayDisplayCommand(uploadCommand); got != "" {
		t.Fatalf("upload display command duplicated argv: %q", got)
	}
}
