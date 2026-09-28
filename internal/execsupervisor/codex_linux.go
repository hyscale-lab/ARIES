//go:build linux

package execsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	supervisorProofPrefix = "\x1eARIES_CODEX_REAPED_"
	supervisorProofSuffix = "\x1f"
	supervisorNonceBytes  = 64
	supervisorFailureCode = 125
	supervisorGrace       = 5 * time.Second
	supervisorCleanup     = 5 * time.Second
	supervisorStagePrefix = "/.aries-codex-"
)

type supervisorInvocation struct {
	codex       string
	stageDir    string
	user        string
	cleanupOnly bool
}

// RunSupervisor is the Codex mode of the standalone Linux aries-exec helper. It
// must run in a dedicated process, because becoming a child subreaper changes
// process-wide child ownership. It runs as root with --codex STAGE/codex
// --stage-dir STAGE --user USER; omitted USER defaults to 0:0. STAGE must be the
// root-level /.aries-codex-<32 lowercase hex characters> directory. The first
// stdin record is a 64-character lowercase hexadecimal nonce and newline; it is
// consumed privately, then the same descriptor carries native Codex JSON-RPC.
// --cleanup-stage STAGE performs only trusted Go removal before agent access.
//
// The nonce never appears in argv or the native server environment. Dumpability
// is disabled, privilege acquisition is prohibited, and capabilities that could
// bypass that boundary are rejected before native code runs. The caller supplies
// only the task environment, not harness credentials. Native argv is always
// exactly exec-server --listen stdio.
//
// A zero exit code and the exact terminal stderr proof together confirm that
// every descendant was reaped and the stage was removed without running task
// executables. The native child receives the task's configured identity; the
// supervisor remains root. Native failure is reported separately. A killed
// supervisor or incomplete cleanup produces no valid successful proof.
func RunSupervisor(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	invocation, err := parseSupervisorArguments(args)
	if err != nil {
		return supervisorFailureCode, err
	}
	if os.Geteuid() != 0 {
		return supervisorFailureCode, errors.New("Codex supervisor must run as root")
	}
	if invocation.cleanupOnly {
		cleanupCtx, cancel := context.WithTimeout(ctx, supervisorCleanup)
		defer cancel()
		if err := removeSupervisorStage(cleanupCtx, invocation.stageDir); err != nil {
			return supervisorFailureCode, err
		}
		return 0, nil
	}
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return supervisorFailureCode, fmt.Errorf("read Codex task users: %w", err)
	}
	groups, err := os.ReadFile("/etc/group")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return supervisorFailureCode, fmt.Errorf("read Codex task groups: %w", err)
	}
	credential, err := resolveSupervisorCredential(invocation.user, passwd, groups)
	if err != nil {
		return supervisorFailureCode, err
	}
	return runSupervisor(ctx, invocation, credential, stdin, stdout, stderr)
}

func parseSupervisorArguments(args []string) (supervisorInvocation, error) {
	if len(args) == 2 && args[0] == "--cleanup-stage" && validSupervisorStage(args[1]) {
		return supervisorInvocation{stageDir: args[1], cleanupOnly: true}, nil
	}
	if len(args) != 4 && len(args) != 6 || args[0] != "--codex" || args[2] != "--stage-dir" || !validSupervisorStage(args[3]) || args[1] != args[3]+"/codex" {
		return supervisorInvocation{}, errors.New("Codex supervisor requires --codex STAGE/codex --stage-dir STAGE [--user USER], or --cleanup-stage STAGE")
	}
	user := "0:0"
	if len(args) == 6 {
		if args[4] != "--user" || args[5] == "" || strings.ContainsAny(args[5], "\x00\r\n") {
			return supervisorInvocation{}, errors.New("Codex supervisor user is invalid")
		}
		user = args[5]
	}
	return supervisorInvocation{codex: args[1], stageDir: args[3], user: user}, nil
}

func validSupervisorStage(stage string) bool {
	if !strings.HasPrefix(stage, supervisorStagePrefix) || len(stage) != len(supervisorStagePrefix)+32 || filepath.Clean(stage) != stage {
		return false
	}
	for _, value := range stage[len(supervisorStagePrefix):] {
		if value >= '0' && value <= '9' || value >= 'a' && value <= 'f' {
			continue
		}
		return false
	}
	return true
}

func runSupervisor(ctx context.Context, invocation supervisorInvocation, credential *syscall.Credential, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	if invocation.stageDir == "" || !filepath.IsAbs(invocation.stageDir) || filepath.Clean(invocation.stageDir) != invocation.stageDir || invocation.stageDir == "/" {
		return supervisorFailureCode, errors.New("Codex supervisor stage is invalid")
	}
	if stdin == nil || stdout == nil || stderr == nil {
		return supervisorFailureCode, errors.New("Codex supervisor requires stdin, stdout, and stderr")
	}
	info, err := os.Lstat(invocation.codex)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return supervisorFailureCode, errors.New("Codex supervisor executable must be a regular executable file")
	}
	// no_new_privs and capabilities are per-thread. Spawn from the exact thread
	// whose irreversible privilege boundary is established and checked here.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	releaseProtection, err := protectSupervisor()
	if err != nil {
		return supervisorFailureCode, err
	}
	defer releaseProtection()
	nonce, err := readSupervisorNonce(ctx, stdin)
	if err != nil {
		return supervisorFailureCode, err
	}
	server := exec.Command(invocation.codex, "exec-server", "--listen", "stdio")
	if credential != nil {
		server.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	server.Stdin, server.Stdout, server.Stderr = stdin, stdout, stderr
	server.WaitDelay = supervisorCleanup
	var serverErr error
	cleanupCtx := context.Background()
	cleanupCancel := func() {}
	if err := server.Start(); err != nil {
		serverErr = err
		cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), supervisorCleanup)
	} else {
		waited := make(chan error, 1)
		go func() { waited <- server.Wait() }()
		select {
		case serverErr = <-waited:
			cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), supervisorCleanup)
		case <-ctx.Done():
			// This fresh budget covers both graceful shutdown and forced reaping.
			// Wait must finish before the generic child reaper is allowed to run.
			cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), supervisorGrace+supervisorCleanup)
			serverErr, err = stopSupervisorServer(cleanupCtx, server, waited)
		}
	}
	defer cleanupCancel()
	if err != nil {
		return supervisorFailureCode, err
	}
	if err := reapSupervisorChildren(cleanupCtx); err != nil {
		return supervisorFailureCode, err
	}
	if err := removeSupervisorStage(cleanupCtx, invocation.stageDir); err != nil {
		return supervisorFailureCode, err
	}
	if serverErr != nil {
		message := "aries-exec: exec-server failed to start\n"
		if server.ProcessState != nil {
			message = fmt.Sprintf("aries-exec: exec-server exited with status %d\n", server.ProcessState.ExitCode())
		}
		if err := writeSupervisorMessage(stderr, message); err != nil {
			return supervisorFailureCode, err
		}
	}
	if err := writeSupervisorMessage(stderr, supervisorProofPrefix+nonce+supervisorProofSuffix); err != nil {
		return supervisorFailureCode, err
	}
	return 0, nil
}
