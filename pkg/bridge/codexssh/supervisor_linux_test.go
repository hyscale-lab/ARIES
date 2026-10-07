//go:build linux

package codexssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const supervisorTestNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Every subreaper runs in a separate process: setting PR_SET_CHILD_SUBREAPER
// inside the go test process would change ownership for unrelated tests.
func TestSupervisorChild(t *testing.T) {
	mode := os.Getenv("ARIES_SUPERVISOR_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "canceled-reaper" {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		child := exec.Command("/bin/sleep", "60")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := reapSupervisorChildren(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled reaper = %v", err)
		}
		cleanup, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		if err := reapSupervisorChildren(cleanup); err != nil {
			t.Fatal(err)
		}
		_ = child.Process.Release()
		return
	}
	if os.Getenv("ARIES_SUPERVISOR_TEST_IGNORE_SIGCHLD") == "1" {
		signal.Ignore(syscall.SIGCHLD)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	native := os.Getenv("ARIES_SUPERVISOR_TEST_CODEX")
	stage := filepath.Dir(native)
	if replacement := os.Getenv("ARIES_SUPERVISOR_TEST_STAGE"); replacement != "" {
		stage = replacement
	}
	// The production CLI requires a root-owned root-level stage. This private
	// runner fixture exercises process ownership as the host test user without
	// weakening production argument validation or requiring Docker/root.
	code, err := runSupervisor(ctx, supervisorInvocation{codex: native, stageDir: stage}, nil, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func TestSupervisorEOFConfirmsCleanupAndPreservesNativeStreams(t *testing.T) {
	native := supervisorFixture(t, `if readlink /proc/$PPID/fd/0 >/dev/null 2>&1; then
  printf 'supervisor stdin was inspectable\n' >&2
  exit 44
fi
grep -Eq '^NoNewPrivs:[[:space:]]*1$' /proc/self/status || exit 45
printf 'task=%s\n' "$ARIES_TASK_FIXTURE"
cat
printf 'native stderr\n' >&2
exit 7
`)
	command, stdout, stderr := supervisorCommand(t, native)
	command.Env = append(command.Env, "ARIES_TASK_FIXTURE=task-owned-value")
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n" + "native RPC input\n")
	if err := command.Run(); err != nil {
		t.Fatalf("supervisor = %v, stderr %q", err, stderr.String())
	}
	if stdout.String() != "task=task-owned-value\nnative RPC input\n" {
		t.Fatalf("native stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "native stderr\n") || !strings.Contains(stderr.String(), "exec-server exited with status 7\n") {
		t.Fatalf("native outcome was not retained: %q", stderr.String())
	}
	assertSupervisorProof(t, stderr.Bytes())
	if _, err := os.Lstat(filepath.Dir(native)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage remained after cleanup proof: %v", err)
	}
	if bytes.Contains(stdout.Bytes(), []byte(supervisorTestNonce)) || strings.Count(stderr.String(), supervisorTestNonce) != 1 {
		t.Fatal("proof nonce was forwarded to native streams")
	}
}

func TestSupervisorReapsEscapedDescendantsAndPreservesUnrelatedProcess(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is required for the escaped-process regression")
	}
	directory := t.TempDir()
	descendant := filepath.Join(directory, "escaped")
	childIdentity := filepath.Join(directory, "escaped.stat")
	grandchildIdentity := filepath.Join(directory, "grandchild.stat")
	writeSupervisorScript(t, descendant, `trap '' TERM
read -r identity < /proc/$$/stat
printf '%s\n' "$identity" > `+supervisorShellQuote(childIdentity)+`
setsid /bin/sh -c 'trap "" TERM; read -r identity < /proc/$$/stat; printf "%s\n" "$identity" > "$1"; exec /bin/sleep 60' aries-test `+supervisorShellQuote(grandchildIdentity)+` &
wait
`)
	intermediate := filepath.Join(directory, "intermediate")
	writeSupervisorScript(t, intermediate, "setsid "+supervisorShellQuote(descendant)+" >/dev/null 2>&1 &\nexit 0\n")
	native := supervisorFixture(t, "setsid "+supervisorShellQuote(intermediate)+" >/dev/null 2>&1 &\n"+
		"while [ ! -s "+supervisorShellQuote(grandchildIdentity)+" ]; do /bin/sleep 0.01; done\nexit 0\n")
	unrelated := exec.Command("/bin/sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	})
	for _, path := range []string{childIdentity, grandchildIdentity} {
		path := path
		t.Cleanup(func() { killSupervisorFixtureProcess(path) })
	}
	command, _, stderr := supervisorCommand(t, native)
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
	if err := command.Run(); err != nil {
		t.Fatalf("supervisor = %v, stderr %q", err, stderr.String())
	}
	assertSupervisorProof(t, stderr.Bytes())
	for _, path := range []string{childIdentity, grandchildIdentity} {
		identity, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if supervisorFixtureProcessPresent(identity) {
			t.Fatalf("escaped descendant is still present: %s", identity)
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process did not survive: %v", err)
	}
	unrelatedStat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", unrelated.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	state := strings.Fields(string(unrelatedStat)[strings.LastIndex(string(unrelatedStat), ") ")+2:])
	if len(state) == 0 || state[0] == "Z" || state[0] == "X" {
		t.Fatalf("unrelated process was killed but not yet reaped: %s", unrelatedStat)
	}
}

func TestSupervisorSignalStillConfirmsDescendantCleanup(t *testing.T) {
	for _, ignoreTERM := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore_TERM_%t", ignoreTERM), func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			body := ""
			if ignoreTERM {
				body = "trap '' TERM\n"
			}
			native := supervisorFixture(t, body+"printf ready > "+supervisorShellQuote(ready)+"\nexec /bin/sleep 60\n")
			command, _, stderr := supervisorCommand(t, native)
			command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			awaitSupervisorFile(t, ready)
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("supervisor = %v, stderr %q", err, stderr.String())
			}
			assertSupervisorProof(t, stderr.Bytes())
		})
	}
}

func TestSupervisorKilledNativeStillConfirmsCleanup(t *testing.T) {
	native := supervisorFixture(t, "kill -KILL $$\n")
	command, _, stderr := supervisorCommand(t, native)
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
	if err := command.Run(); err != nil {
		t.Fatalf("supervisor = %v, stderr %q", err, stderr.String())
	}
	assertSupervisorProof(t, stderr.Bytes())
}

func TestSupervisorDisablesInheritedChildAutoReaping(t *testing.T) {
	native := supervisorFixture(t, "printf native-ran\n")
	command, stdout, stderr := supervisorCommand(t, native)
	command.Env = append(command.Env, "ARIES_SUPERVISOR_TEST_IGNORE_SIGCHLD=1")
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
	if err := command.Run(); err != nil {
		t.Fatalf("supervisor = %v, stderr %q", err, stderr.String())
	}
	if stdout.String() != "native-ran" || stderr.String() != "\x1eARIES_CODEX_REAPED_"+supervisorTestNonce+"\x1f" {
		t.Fatalf("native child was not waited normally: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestSupervisorDoesNotProveMalformedOrIncompleteNonce(t *testing.T) {
	native := supervisorFixture(t, "printf native-ran\n")
	for _, input := range []string{"", supervisorTestNonce, strings.Repeat("A", 64) + "\n", supervisorTestNonce + "0\n", "\n"} {
		t.Run(strconv.Itoa(len(input))+"-"+strconv.Itoa(strings.Count(input, "A")), func(t *testing.T) {
			command, stdout, stderr := supervisorCommand(t, native)
			command.Stdin = strings.NewReader(input)
			if err := command.Run(); err == nil {
				t.Fatal("malformed nonce was accepted")
			}
			if stdout.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("ARIES_CODEX_REAPED_")) {
				t.Fatalf("invalid input ran native code or produced proof: stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestSupervisorCancellationWhileAwaitingNonceDoesNotProveCleanup(t *testing.T) {
	native := supervisorFixture(t, "printf native-ran\n")
	command, stdout, stderr := supervisorCommand(t, native)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// A partial nonce ensures the helper must wait for input, not a child.
	if _, err := input.Write([]byte(supervisorTestNonce[:1])); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted nonce read succeeded")
	}
	if stdout.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("ARIES_CODEX_REAPED_")) {
		t.Fatalf("interrupted input ran native code or produced proof: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestSupervisorRejectsRewindableNonceInput(t *testing.T) {
	native := supervisorFixture(t, "printf native-ran\n")
	inputPath := filepath.Join(t.TempDir(), "nonce")
	if err := os.WriteFile(inputPath, []byte(supervisorTestNonce+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	command, stdout, stderr := supervisorCommand(t, native)
	command.Stdin = input
	if err := command.Run(); err == nil {
		t.Fatal("rewindable nonce input was accepted")
	}
	if stdout.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("ARIES_CODEX_REAPED_")) {
		t.Fatalf("rewindable input ran native code or produced proof: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestSupervisorFailedProofWriteCannotSucceed(t *testing.T) {
	native := supervisorFixture(t, "exit 0\n")
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	command, _, _ := supervisorCommand(t, native)
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
	command.Stderr = full
	if err := command.Run(); err == nil {
		t.Fatal("failed cleanup-proof write was reported as success")
	}
}

func TestSupervisorReaperHonorsCancellation(t *testing.T) {
	command, _, stderr := supervisorCommand(t, "")
	command.Env = append(command.Env, "ARIES_SUPERVISOR_TEST_MODE=canceled-reaper")
	if err := command.Run(); err != nil {
		t.Fatalf("canceled-reaper regression = %v: %s", err, stderr.String())
	}
}

func TestSupervisorRejectsUnsafeCapabilities(t *testing.T) {
	const safe = "CapPrm:\t0\nCapEff:\t0\nCapInh:\t0\nCapAmb:\t0\nCapBnd:\tffffffffffffffff\nNoNewPrivs:\t1\n"
	invalid := []string{"", safe + "CapPrm:\t0\n", strings.Replace(safe, "CapEff:\t0", "CapEff:\tnot-hex", 1), strings.Replace(safe, "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)}
	for _, name := range []string{"CapPrm", "CapEff", "CapInh", "CapAmb"} {
		for _, mask := range []string{"0000000000200000", "0000000000080000"} {
			invalid = append(invalid, strings.Replace(safe, name+":\t0", name+":\t"+mask, 1))
		}
		invalid = append(invalid, strings.Replace(safe, name+":\t0\n", "", 1))
	}
	for _, content := range invalid {
		if err := validateSupervisorCapabilities([]byte(content)); err == nil {
			t.Fatalf("accepted unsafe capability status %q", content)
		}
	}
	if err := validateSupervisorCapabilities([]byte(safe)); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorProductionArgumentsRequireOwnedStage(t *testing.T) {
	stage := "/.aries-codex-0123456789abcdef0123456789abcdef"
	valid := []string{"--codex", stage + "/codex", "--stage-dir", stage, "--user", "65532:65532"}
	parsed, err := parseSupervisorArguments(valid)
	if err != nil || parsed.codex != stage+"/codex" || parsed.stageDir != stage || parsed.user != "65532:65532" || parsed.cleanupOnly {
		t.Fatalf("normal arguments = %+v, %v", parsed, err)
	}
	parsed, err = parseSupervisorArguments(valid[:4])
	if err != nil || parsed.user != "0:0" {
		t.Fatalf("default identity = %+v, %v", parsed, err)
	}
	parsed, err = parseSupervisorArguments([]string{"--cleanup-stage", stage})
	if err != nil || !parsed.cleanupOnly || parsed.stageDir != stage {
		t.Fatalf("cleanup arguments = %+v, %v", parsed, err)
	}
	for _, invalid := range [][]string{
		nil, {"--codex", stage + "/codex"},
		{"--cleanup-stage", "/"}, {"--cleanup-stage", stage + "/"},
		{"--cleanup-stage", stage + "/../victim"}, {"--cleanup-stage", "/tmp" + stage},
		{"--cleanup-stage", "/.aries-codex-" + strings.Repeat("A", 32)},
		{"--codex", stage + "/other", "--stage-dir", stage},
		{"--codex", stage + "/codex", "--stage-dir", stage, "--user", ""},
		{"--cleanup-stage", stage, "--user", "0:0"},
	} {
		if _, err := parseSupervisorArguments(invalid); err == nil {
			t.Fatalf("unsafe arguments accepted: %q", invalid)
		}
	}
}

func TestSupervisorProductionRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root production rejection requires a non-root host user")
	}
	stage := "/.aries-codex-0123456789abcdef0123456789abcdef"
	code, err := RunSupervisor(context.Background(), []string{"--codex", stage + "/codex", "--stage-dir", stage}, os.Stdin, os.Stdout, os.Stderr)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("unprivileged production invocation = %d, %v", code, err)
	}
}

func TestSupervisorResolvesTaskIdentityWithoutAddingPrivileges(t *testing.T) {
	passwd := []byte("root:x:0:0::/root:/bin/sh\nagent:x:1234:2345::/home/agent:/bin/sh\n")
	groups := []byte("root:x:0:\nagent:x:2345:agent\nproject:x:3456:agent,other\nother:x:4567:other\n")
	for _, test := range []struct {
		user string
		want syscall.Credential
	}{
		{"0:0", syscall.Credential{Uid: 0, Gid: 0}},
		{"65532:65532", syscall.Credential{Uid: 65532, Gid: 65532}},
		{"agent", syscall.Credential{Uid: 1234, Gid: 2345, Groups: []uint32{3456}}},
		{"1234", syscall.Credential{Uid: 1234, Gid: 2345, Groups: []uint32{3456}}},
		{"9876", syscall.Credential{Uid: 9876, Gid: 0}},
		{"agent:root", syscall.Credential{Uid: 1234, Gid: 0}},
		{"agent:9999", syscall.Credential{Uid: 1234, Gid: 9999}},
		{"9876:project", syscall.Credential{Uid: 9876, Gid: 3456}},
	} {
		got, err := resolveSupervisorCredential(test.user, passwd, groups)
		if err != nil || !reflect.DeepEqual(*got, test.want) {
			t.Fatalf("identity %q = %+v, %v; want %+v", test.user, got, err, test.want)
		}
	}
	for _, user := range []string{"", ":0", "root:", "root:0:0", "unknown", "agent:unknown", "4294967295:0", "0:4294967295", "agent\n"} {
		if _, err := resolveSupervisorCredential(user, passwd, groups); err == nil {
			t.Fatalf("invalid identity %q accepted", user)
		}
	}
	if _, err := resolveSupervisorCredential("65532:65532", nil, nil); err != nil {
		t.Fatalf("numeric identity requires nonexistent account files: %v", err)
	}
}

func TestSupervisorStageCleanupDoesNotFollowSymlink(t *testing.T) {
	outside := t.TempDir()
	marker := filepath.Join(outside, "preserved")
	if err := os.WriteFile(marker, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rootLink := range []bool{false, true} {
		stage := filepath.Join(t.TempDir(), "stage")
		if !rootLink {
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		link := stage
		if !rootLink {
			link = filepath.Join(stage, "outside")
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := removeSupervisorStage(context.Background(), stage); err != nil {
			t.Fatal(err)
		}
		if content, err := os.ReadFile(marker); err != nil || string(content) != "unrelated" {
			t.Fatalf("cleanup followed a stage symlink: %q, %v", content, err)
		}
		if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage still exists: %v", err)
		}
	}
}

func TestSupervisorStageCleanupFailureSuppressesProof(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure fixture requires a non-root host user")
	}
	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	native := supervisorFixture(t, "exit 0\n")
	command, _, stderr := supervisorCommand(t, native)
	command.Env = append(command.Env, "ARIES_SUPERVISOR_TEST_STAGE="+stage)
	command.Stdin = strings.NewReader(supervisorTestNonce + "\n")
	if err := command.Run(); err == nil {
		t.Fatal("failed stage cleanup was accepted")
	}
	if bytes.Contains(stderr.Bytes(), []byte("ARIES_CODEX_REAPED_")) {
		t.Fatalf("failed stage cleanup produced proof: %q", stderr.String())
	}
}

func supervisorCommand(t *testing.T, native string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorChild$")
	command.Env = append(os.Environ(), "ARIES_SUPERVISOR_TEST_MODE=run", "ARIES_SUPERVISOR_TEST_CODEX="+native)
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	command.Stdout, command.Stderr = stdout, stderr
	return command, stdout, stderr
}

func supervisorFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native codex")
	writeSupervisorScript(t, path, `if [ "$#" -ne 3 ] || [ "$1" != exec-server ] || [ "$2" != --listen ] || [ "$3" != stdio ]; then
  printf 'unexpected native argv\n' >&2
  exit 43
fi
`+body)
	return path
}

func writeSupervisorScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func supervisorShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func assertSupervisorProof(t *testing.T, content []byte) {
	t.Helper()
	proof := []byte("\x1eARIES_CODEX_REAPED_" + supervisorTestNonce + "\x1f")
	if !bytes.HasSuffix(content, proof) || bytes.Count(content, proof) != 1 {
		t.Fatalf("missing unique terminal cleanup proof: %q", content)
	}
}

func awaitSupervisorFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("native process did not write %s", path)
}

func supervisorFixtureIdentity(content []byte) (int, string) {
	text := string(content)
	open, close := strings.IndexByte(text, '('), strings.LastIndex(text, ") ")
	if open < 1 || close < open {
		return 0, ""
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(text[:open]))
	fields := strings.Fields(text[close+2:])
	if len(fields) < 20 {
		return 0, ""
	}
	return pid, fields[19]
}

func supervisorFixtureProcessPresent(identity []byte) bool {
	pid, started := supervisorFixtureIdentity(identity)
	if pid <= 1 || started == "" {
		return true
	}
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	currentPID, currentStart := supervisorFixtureIdentity(content)
	return currentPID == pid && currentStart == started
}

func killSupervisorFixtureProcess(path string) {
	identity, err := os.ReadFile(path)
	if err != nil || !supervisorFixtureProcessPresent(identity) {
		return
	}
	pid, _ := supervisorFixtureIdentity(identity)
	if pid > 1 {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
}
