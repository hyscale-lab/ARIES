//go:build apptainer

package apptainer

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Hermes saves a command's environment and restores it in the next. The key it
// restores must still reach a daemon, or the command blocks once the queue is full.
func TestRealCommandsKeepAWorkingFakerootKey(t *testing.T) {
	if _, err := exec.LookPath("fakeroot-sysv"); err != nil {
		t.Skip("no fakeroot-sysv on PATH, so Apptainer injects no fakeroot")
	}
	manager := newRealManager(t)
	ctx := context.Background()
	id, err := manager.Create(ctx, realRequest(t))
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), id); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := func(script string) string {
		t.Helper()
		result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", script}, Timeout: time.Minute})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%q = %+v, %v", script, result, err)
		}
		return strings.TrimSpace(result.Stdout)
	}
	run(`export -p > /tmp/saved-environment`)
	// Each chmod sends the daemon a message and expects no reply, so a queue that
	// nothing reads fills after a few of them.
	script := `. /tmp/saved-environment; : > /tmp/file; i=0; while [ "$i" -lt 64 ]; do chmod 640 /tmp/file; i=$((i + 1)); done; echo finished`
	if got := run(script); got != "finished" {
		t.Fatalf("restored environment: %q", got)
	}
	// Ownership a command fakes stays visible to the next.
	run(`: > /tmp/owned; chown 1234:1234 /tmp/owned`)
	if got := run(`stat -c %u /tmp/owned`); got != "1234" {
		t.Fatalf("faked owner in a later command = %q", got)
	}
}
