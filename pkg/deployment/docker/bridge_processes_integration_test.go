package docker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

func TestLiveAssignmentRevokeKillsDetachedProcessesPreservingBaseline(t *testing.T) {
	if os.Getenv("ARIES_TEST_DOCKER") != "1" {
		t.Skip("ARIES_TEST_DOCKER not enabled")
	}
	image := os.Getenv("ARIES_TEST_SANDBOX_IMAGE")
	if image == "" {
		image = "alexgshaw/fix-git:20260403"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	env := manager.NewTaskEnvironment()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := env.Stop(cleanup); err != nil {
			t.Error(err)
		}
	}()
	connectivity, err := env.Start(ctx, core.SandboxRequest{RunID: "bridge-process-test", TaskID: "detached", Environment: core.Environment{AllowNetwork: false}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := manager.Create(ctx, deployment.Request{Name: fmt.Sprintf("aries-detached-test-%d", time.Now().UnixNano()), Image: image, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Init: true, Placement: connectivity.Placement})
	if id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := manager.Stop(cleanup, id); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	exec := func(command string) core.CommandResult {
		t.Helper()
		result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", command}, User: "0:0"})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("command failed %+v %v", result, err)
		}
		return result
	}
	original := strings.TrimSpace(exec(`setsid /bin/sleep 300 </dev/null >/dev/null 2>&1 & echo "$!"`).Stdout)
	if _, err = strconv.Atoi(original); err != nil {
		t.Fatal(err)
	}
	baseline, err := manager.SnapshotBridgeProcesses(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	detached := strings.TrimSpace(exec(`setsid /bin/sh -c 'sleep 300; echo leaked >/tmp/aries-late-mutation' </dev/null >/dev/null 2>&1 & echo "$!"`).Stdout)
	if _, err = strconv.Atoi(detached); err != nil {
		t.Fatal(err)
	}
	exec("kill -0 " + detached)
	if err = manager.RevokeBridgeProcesses(ctx, id, baseline); err != nil {
		t.Fatal(err)
	}
	exec("kill -0 " + original + "; test ! -e /tmp/aries-late-mutation; if [ -e /proc/" + detached + "/stat ]; then state=$(cat /proc/" + detached + "/stat); case \"$state\" in *') Z '*) :;; *) exit 1;; esac; fi")
	if err = manager.RevokeBridgeProcesses(ctx, id, baseline); err != nil {
		t.Fatal("repeated process revocation:", err)
	}
	after, err := manager.SnapshotBridgeProcesses(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	preserved := map[deployment.ProcessIdentity]bool{}
	for _, identity := range baseline {
		preserved[identity] = true
	}
	for _, identity := range after {
		if !preserved[identity] {
			t.Fatalf("nonbaseline process survived revocation: %+v", identity)
		}
	}
}
