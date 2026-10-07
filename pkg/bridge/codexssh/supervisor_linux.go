//go:build linux

package codexssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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

// RunSupervisor is the standalone Linux entry point for aries-codex-exec. It
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
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return supervisorFailureCode, fmt.Errorf("protect Codex supervisor memory: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return supervisorFailureCode, fmt.Errorf("prevent Codex supervisor privilege acquisition: %w", err)
	}
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return supervisorFailureCode, fmt.Errorf("read Codex supervisor capability boundary: %w", err)
	}
	if err := validateSupervisorCapabilities(status); err != nil {
		return supervisorFailureCode, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return supervisorFailureCode, fmt.Errorf("establish Codex child ownership: %w", err)
	}
	// An inherited SIG_IGN disposition could auto-reap children and permit
	// PID reuse between enumeration and signalling. Install a handler which
	// never reaps; Cmd.Wait and then our Wait4 loop remain the sole consumers.
	childSignals := make(chan os.Signal, 1)
	signal.Notify(childSignals, syscall.SIGCHLD)
	defer signal.Stop(childSignals)
	children, err := supervisorChildren()
	if err != nil || len(children) != 0 {
		return supervisorFailureCode, errors.Join(errors.New("Codex supervisor must start without existing children"), err)
	}
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
		message := "aries-codex-exec: exec-server failed to start\n"
		if server.ProcessState != nil {
			message = fmt.Sprintf("aries-codex-exec: exec-server exited with status %d\n", server.ProcessState.ExitCode())
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

func resolveSupervisorCredential(user string, passwd, groups []byte) (*syscall.Credential, error) {
	if user == "" || strings.ContainsAny(user, "\x00\r\n") || strings.Count(user, ":") > 1 {
		return nil, errors.New("Codex task identity must be USER or USER:GROUP")
	}
	userName, groupName, explicitGroup := strings.Cut(user, ":")
	if userName == "" || explicitGroup && groupName == "" {
		return nil, errors.New("Codex task identity contains an empty user or group")
	}
	uid, numericUser, err := supervisorNumericID(userName)
	if err != nil {
		return nil, err
	}
	credential := &syscall.Credential{Uid: uid}
	if numericUser && explicitGroup {
		gid, numericGroup, err := supervisorNumericID(groupName)
		if err != nil {
			return nil, err
		}
		if numericGroup {
			credential.Gid = gid
			return credential, nil
		}
	}
	matchedName := ""
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || strings.HasPrefix(line, "#") {
			continue
		}
		entryUID, numeric, parseErr := supervisorNumericID(fields[2])
		if numericUser && (parseErr != nil || !numeric || entryUID != uid) || !numericUser && fields[0] != userName {
			continue
		}
		entryGID, gidNumeric, gidErr := supervisorNumericID(fields[3])
		if parseErr != nil || !numeric || gidErr != nil || !gidNumeric {
			return nil, errors.New("Codex task passwd identity is invalid")
		}
		credential.Uid, credential.Gid = entryUID, entryGID
		matchedName = fields[0]
		break
	}
	if !numericUser && matchedName == "" {
		return nil, errors.New("Codex task user is absent from /etc/passwd")
	}
	if explicitGroup {
		gid, numeric, err := supervisorNumericID(groupName)
		if err != nil {
			return nil, err
		}
		if numeric {
			credential.Gid = gid
			return credential, nil
		}
	}
	seenGroups := make(map[uint32]bool)
	for _, line := range strings.Split(string(groups), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || strings.HasPrefix(line, "#") {
			continue
		}
		matches := explicitGroup && fields[0] == groupName || !explicitGroup && matchedName != "" && slices.Contains(strings.Split(fields[3], ","), matchedName)
		if !matches {
			continue
		}
		gid, numeric, err := supervisorNumericID(fields[2])
		if err != nil || !numeric {
			return nil, errors.New("Codex task group identity is invalid")
		}
		if explicitGroup {
			credential.Gid = gid
			return credential, nil
		}
		if gid != credential.Gid && !seenGroups[gid] {
			credential.Groups = append(credential.Groups, gid)
			seenGroups[gid] = true
		}
	}
	if explicitGroup {
		return nil, errors.New("Codex task group is absent from /etc/group")
	}
	return credential, nil
}

func supervisorNumericID(value string) (uint32, bool, error) {
	if value == "" {
		return 0, false, nil
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false, nil
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == uint64(^uint32(0)) {
		return 0, true, errors.New("Codex task numeric identity is out of range")
	}
	return uint32(parsed), true, nil
}

func removeSupervisorStage(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("remove Codex supervisor stage: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		err := os.RemoveAll(stage)
		if err == nil {
			_, statErr := os.Lstat(stage)
			if !errors.Is(statErr, os.ErrNotExist) {
				err = errors.Join(errors.New("Codex supervisor stage absence was not confirmed"), statErr)
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("remove Codex supervisor stage: %w", err)
		}
		return nil
	case <-ctx.Done():
		// This entry point owns its entire process. The command exits nonzero,
		// ending a blocked filesystem operation without emitting a proof.
		return fmt.Errorf("remove Codex supervisor stage: %w", ctx.Err())
	}
}

func validateSupervisorCapabilities(status []byte) error {
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(status), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch name {
		case "CapPrm", "CapEff", "CapInh", "CapAmb", "NoNewPrivs":
		default:
			continue
		}
		if seen[name] {
			return fmt.Errorf("Codex supervisor capability boundary contains duplicate %s", name)
		}
		seen[name] = true
		if name == "NoNewPrivs" {
			if strings.TrimSpace(value) != "1" {
				return errors.New("Codex supervisor privilege acquisition is not disabled")
			}
			continue
		}
		capabilities, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			return errors.New("Codex supervisor capability boundary is invalid")
		}
		if capabilities&((1<<unix.CAP_SYS_PTRACE)|(1<<unix.CAP_SYS_ADMIN)) != 0 {
			return fmt.Errorf("Codex supervisor requires CAP_SYS_PTRACE and CAP_SYS_ADMIN absent from %s", name)
		}
	}
	for _, name := range []string{"CapPrm", "CapEff", "CapInh", "CapAmb", "NoNewPrivs"} {
		if !seen[name] {
			return fmt.Errorf("Codex supervisor capability boundary is missing %s", name)
		}
	}
	// Bounding capabilities alone confer no authority. With no_new_privs set,
	// exec cannot add anything absent from the permitted set, even if CapBnd
	// is broad (as is normal for an unprivileged host user).
	return nil
}

func readSupervisorNonce(ctx context.Context, stdin *os.File) (string, error) {
	var record [supervisorNonceBytes + 1]byte
	fd := int(stdin.Fd())
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return "", fmt.Errorf("inspect Codex supervisor stdin: %w", err)
	}
	if kind := info.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		// A regular file could be rewound by the native child to recover the
		// nonce. A terminal could echo it. Docker's non-TTY exec uses a pipe.
		return "", errors.New("Codex supervisor stdin must be a pipe or socket")
	}
	for read := 0; read < len(record); {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("read Codex supervisor proof nonce: %w", err)
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		count, err := unix.Poll(poll, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("await Codex supervisor proof nonce: %w", err)
		}
		if count == 0 {
			continue
		}
		if poll[0].Revents&unix.POLLNVAL != 0 {
			return "", errors.New("Codex supervisor stdin is invalid")
		}
		count, err = unix.Read(fd, record[read:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read Codex supervisor proof nonce: %w", err)
		}
		if count == 0 {
			return "", errors.New("Codex supervisor proof nonce is incomplete")
		}
		read += count
	}
	if record[supervisorNonceBytes] != '\n' {
		return "", errors.New("Codex supervisor proof nonce must be one bounded line")
	}
	for _, value := range record[:supervisorNonceBytes] {
		if value >= '0' && value <= '9' || value >= 'a' && value <= 'f' {
			continue
		}
		return "", errors.New("Codex supervisor proof nonce must be lowercase hexadecimal")
	}
	return string(record[:supervisorNonceBytes]), nil
}

func stopSupervisorServer(ctx context.Context, server *exec.Cmd, waited <-chan error) (error, error) {
	if err := server.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, fmt.Errorf("terminate Codex exec-server: %w", err)
	}
	grace := time.NewTimer(supervisorGrace)
	defer grace.Stop()
	select {
	case result := <-waited:
		return result, nil
	case <-grace.C:
	case <-ctx.Done():
		return nil, fmt.Errorf("await Codex exec-server shutdown: %w", ctx.Err())
	}
	if err := server.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, fmt.Errorf("kill Codex exec-server: %w", err)
	}
	select {
	case result := <-waited:
		return result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("confirm Codex exec-server exit: %w", ctx.Err())
	}
}

func reapSupervisorChildren(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reap Codex descendants: %w", err)
		}
		children, err := supervisorChildren()
		if err != nil {
			return err
		}
		// We are the sole reaper here. Every listed direct child retains its PID
		// until our later Wait4, so signalling this snapshot cannot hit a reused
		// PID. Killing a parent adopts any escaped descendants for the next pass.
		for _, pid := range children {
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill adopted Codex child: %w", err)
			}
		}
		for {
			var status unix.WaitStatus
			// __WALL also includes clone children with a non-SIGCHLD exit
			// signal; ordinary wait semantics cannot prove their absence.
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG|unix.WALL, nil)
			if errors.Is(err, unix.ECHILD) {
				return nil
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return fmt.Errorf("wait for adopted Codex children: %w", err)
			}
			if pid == 0 {
				break
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("reap Codex descendants: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func supervisorChildren() ([]int, error) {
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("list Codex supervisor threads: %w", err)
	}
	children := make(map[int]struct{})
	for _, thread := range threads {
		content, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue // An exited Go thread's children move to a remaining thread.
		}
		if err != nil {
			return nil, fmt.Errorf("read Codex supervisor children: %w", err)
		}
		for _, value := range strings.Fields(string(content)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 1 || pid == os.Getpid() {
				return nil, errors.New("Codex supervisor has an invalid direct child identity")
			}
			children[pid] = struct{}{}
		}
	}
	result := make([]int, 0, len(children))
	for pid := range children {
		result = append(result, pid)
	}
	slices.Sort(result)
	return result, nil
}

func writeSupervisorMessage(stderr io.Writer, message string) error {
	written, err := io.WriteString(stderr, message)
	if err == nil && written != len(message) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write Codex supervisor outcome: %w", err)
	}
	return nil
}
