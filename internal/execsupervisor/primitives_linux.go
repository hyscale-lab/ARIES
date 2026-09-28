//go:build linux

package execsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// protectSupervisor must run on the locked OS thread used to start children.
// The caller keeps that thread locked until all child starts have completed.
func protectSupervisor() (func(), error) {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("protect agent supervisor memory: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("prevent agent supervisor privilege acquisition: %w", err)
	}
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return nil, fmt.Errorf("read agent supervisor capability boundary: %w", err)
	}
	if err := validateSupervisorCapabilities(status); err != nil {
		return nil, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("establish agent child ownership: %w", err)
	}
	// An inherited SIG_IGN disposition could auto-reap children and permit
	// PID reuse between enumeration and signalling. Install a handler which
	// never reaps; Cmd.Wait and then our Wait4 loop remain the sole consumers.
	childSignals := make(chan os.Signal, 1)
	signal.Notify(childSignals, syscall.SIGCHLD)
	children, err := supervisorChildren()
	if err != nil || len(children) != 0 {
		signal.Stop(childSignals)
		return nil, errors.Join(errors.New("supervisor must start without existing children"), err)
	}
	return func() { signal.Stop(childSignals) }, nil
}

func resolveSupervisorCredential(user string, passwd, groups []byte) (*syscall.Credential, error) {
	if user == "" || strings.ContainsAny(user, "\x00\r\n") || strings.Count(user, ":") > 1 {
		return nil, errors.New("agent task identity must be USER or USER:GROUP")
	}
	userName, groupName, explicitGroup := strings.Cut(user, ":")
	if userName == "" || explicitGroup && groupName == "" {
		return nil, errors.New("agent task identity contains an empty user or group")
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
			return nil, errors.New("agent task passwd identity is invalid")
		}
		credential.Uid, credential.Gid = entryUID, entryGID
		matchedName = fields[0]
		break
	}
	if !numericUser && matchedName == "" {
		return nil, errors.New("agent task user is absent from /etc/passwd")
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
			return nil, errors.New("agent task group identity is invalid")
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
		return nil, errors.New("agent task group is absent from /etc/group")
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
		return 0, true, errors.New("agent task numeric identity is out of range")
	}
	return uint32(parsed), true, nil
}

func removeSupervisorStage(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("remove agent supervisor stage: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		err := os.RemoveAll(stage)
		if err == nil {
			_, statErr := os.Lstat(stage)
			if !errors.Is(statErr, os.ErrNotExist) {
				err = errors.Join(errors.New("agent supervisor stage absence was not confirmed"), statErr)
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("remove agent supervisor stage: %w", err)
		}
		return nil
	case <-ctx.Done():
		// This entry point owns its entire process. The command exits nonzero,
		// ending a blocked filesystem operation without emitting a proof.
		return fmt.Errorf("remove agent supervisor stage: %w", ctx.Err())
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
			return fmt.Errorf("agent supervisor capability boundary contains duplicate %s", name)
		}
		seen[name] = true
		if name == "NoNewPrivs" {
			if strings.TrimSpace(value) != "1" {
				return errors.New("agent supervisor privilege acquisition is not disabled")
			}
			continue
		}
		capabilities, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			return errors.New("agent supervisor capability boundary is invalid")
		}
		if capabilities&((1<<unix.CAP_SYS_PTRACE)|(1<<unix.CAP_SYS_ADMIN)) != 0 {
			return fmt.Errorf("agent supervisor requires CAP_SYS_PTRACE and CAP_SYS_ADMIN absent from %s", name)
		}
	}
	for _, name := range []string{"CapPrm", "CapEff", "CapInh", "CapAmb", "NoNewPrivs"} {
		if !seen[name] {
			return fmt.Errorf("agent supervisor capability boundary is missing %s", name)
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
		return "", fmt.Errorf("inspect agent supervisor stdin: %w", err)
	}
	if kind := info.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		// A regular file could be rewound by the native child to recover the
		// nonce. A terminal could echo it. Docker's non-TTY exec uses a pipe.
		return "", errors.New("agent supervisor stdin must be a pipe or socket")
	}
	for read := 0; read < len(record); {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("read agent supervisor proof nonce: %w", err)
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		count, err := unix.Poll(poll, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("await agent supervisor proof nonce: %w", err)
		}
		if count == 0 {
			continue
		}
		if poll[0].Revents&unix.POLLNVAL != 0 {
			return "", errors.New("agent supervisor stdin is invalid")
		}
		count, err = unix.Read(fd, record[read:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read agent supervisor proof nonce: %w", err)
		}
		if count == 0 {
			return "", errors.New("agent supervisor proof nonce is incomplete")
		}
		read += count
	}
	if record[supervisorNonceBytes] != '\n' {
		return "", errors.New("agent supervisor proof nonce must be one bounded line")
	}
	for _, value := range record[:supervisorNonceBytes] {
		if value >= '0' && value <= '9' || value >= 'a' && value <= 'f' {
			continue
		}
		return "", errors.New("agent supervisor proof nonce must be lowercase hexadecimal")
	}
	return string(record[:supervisorNonceBytes]), nil
}

func stopSupervisorServer(ctx context.Context, server *exec.Cmd, waited <-chan error) (error, error) {
	if err := server.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, fmt.Errorf("terminate supervised command: %w", err)
	}
	grace := time.NewTimer(supervisorGrace)
	defer grace.Stop()
	select {
	case result := <-waited:
		return result, nil
	case <-grace.C:
	case <-ctx.Done():
		return nil, fmt.Errorf("await supervised command shutdown: %w", ctx.Err())
	}
	if err := server.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, fmt.Errorf("kill supervised command: %w", err)
	}
	select {
	case result := <-waited:
		return result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("confirm supervised command exit: %w", ctx.Err())
	}
}

func reapSupervisorChildren(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reap agent descendants: %w", err)
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
				return fmt.Errorf("kill adopted agent child: %w", err)
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
				return fmt.Errorf("wait for adopted agent children: %w", err)
			}
			if pid == 0 {
				break
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("reap agent descendants: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func supervisorChildren() ([]int, error) {
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("list agent supervisor threads: %w", err)
	}
	children := make(map[int]struct{})
	for _, thread := range threads {
		content, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue // An exited Go thread's children move to a remaining thread.
		}
		if err != nil {
			return nil, fmt.Errorf("read agent supervisor children: %w", err)
		}
		for _, value := range strings.Fields(string(content)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 1 || pid == os.Getpid() {
				return nil, errors.New("agent supervisor has an invalid direct child identity")
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
		return fmt.Errorf("write agent supervisor outcome: %w", err)
	}
	return nil
}
