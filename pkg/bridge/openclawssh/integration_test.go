//go:build integration

package openclawssh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"golang.org/x/crypto/ssh"
)

const (
	bridgeFixtureImage  = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"
	pinnedOpenClawImage = "ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3"
	openClawUID         = 1000
	volumeConfigPath    = "/tmp/openclaw-ssh-integration/config"
)

type dockerResult struct {
	stdout, stderr []byte
	exitCode       int
}

func TestOpenClawSSHBridgeRealIsolationAndWorkspaceIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if result := runDocker(ctx, nil, "info"); result.exitCode != 0 {
		t.Fatalf("Docker daemon is required: %s", result.stderr)
	}
	ensureIntegrationImage(t, ctx, bridgeFixtureImage)
	ensureIntegrationImage(t, ctx, pinnedOpenClawImage)
	assertEmptyBridgeInventory(t, ctx)

	outputDir := t.TempDir()
	execHelper := requiredIntegrationHelper(t, "ARIES_EXEC_HELPER")
	clientHelper := requiredIntegrationHelper(t, "ARIES_SSH_CLIENT")
	serverHelper := requiredIntegrationHelper(t, "ARIES_SSH_SERVER")
	manager, err := dockersandbox.New(dockersandbox.Options{
		OutputDir: outputDir, ExecHelperPath: execHelper, CleanupTimeout: 20 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, core.SandboxRequest{
		RunID: "m4-integration-run", TaskID: "m4-integration-task",
		Environment: core.Environment{Image: bridgeFixtureImage, Workdir: "/work", MemoryMB: 64, AllowNetwork: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*dockersandbox.Sandbox)
	bridge, err := New(Options{
		OutputDir: outputDir, ClientPath: clientHelper, ServerPath: serverHelper, CleanupTimeout: 20 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bridgeStarted := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if bridgeStarted {
			if err := bridge.Stop(cleanupCtx); err != nil {
				t.Errorf("bridge cleanup: %v", err)
			}
		}
		if err := sandbox.Stop(cleanupCtx); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
		assertEmptyBridgeInventory(t, cleanupCtx)
	})

	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	bridgeStarted = true
	t.Log("bridge started and readiness probe passed")
	clientInfo, err := os.Stat(endpoint.ClientSourceFile)
	if err != nil || clientInfo.Mode().Perm() != 0o555 {
		t.Fatalf("UID-1000 client source mode = %v, %v", clientInfo, err)
	}
	configSource := writeIntegrationConfig(t)
	volumes := newIntegrationVolumes(t, ctx)
	mainVolume := volumes.create(t, ctx, "main", endpoint.IdentitySourceFile, endpoint.KnownHostsSourceFile, configSource)
	clientSequence := 0
	runClient := func(network, volume, addHost string, stdin []byte, remote []string) dockerResult {
		clientSequence++
		containerName := fmt.Sprintf("aries-m4-client-%d", clientSequence)
		resultDir := t.TempDir()
		stdinPath := filepath.Join(resultDir, "stdin")
		if err := os.WriteFile(stdinPath, stdin, 0o644); err != nil {
			t.Fatal(err)
		}
		remoteCommand := encodeCanonicalTokens(remote)
		wrapper := `set +e; mkdir -p /tmp/aries-result; if [ "$(id -u)" != "1000" ]; then printf 'unexpected uid %s' "$(id -u)" >/tmp/aries-result/stderr; status=126; else "$2" -F "$3" -T -o RequestTTY=no openclaw-sandbox "$4" <"$1" >/tmp/aries-result/stdout 2>/tmp/aries-result/stderr; status=$?; fi; printf '%s' "$status" >/tmp/aries-result/status; exit "$status"`
		args := []string{
			"container", "run", "--detach",
			"--name", containerName,
			"--label", "aries.managed=true", "--label", "aries.milestone=m4", "--label", "aries.kind=bridge-client",
			"--mount", "type=bind,src=" + endpoint.ClientSourceFile + ",dst=" + clientContainerPath + ",readonly",
			"--mount", "type=bind,src=" + stdinPath + ",dst=/tmp/aries-stdin,readonly",
			"--mount", "type=volume,src=" + volume + ",dst=/run/aries/ssh,readonly",
			"--mount", "type=volume,src=" + volume + ",dst=/tmp/openclaw-ssh-integration,readonly",
		}
		if network != "" {
			args = append(args, "--network", network)
		}
		if addHost != "" {
			args = append(args, "--add-host", lockedHostName+":"+addHost)
		}
		args = append(args,
			pinnedOpenClawImage,
			"/bin/sh", "-c", wrapper, "--",
			"/tmp/aries-stdin", clientContainerPath, volumeConfigPath, remoteCommand,
		)
		commandCtx, commandCancel := context.WithTimeout(ctx, 15*time.Second)
		launch := runDocker(commandCtx, nil, args...)
		if launch.exitCode != 0 {
			commandCancel()
			return launch
		}
		result := waitContainerResult(commandCtx, containerName, resultDir)
		commandCancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = runDocker(cleanupCtx, nil, "container", "rm", "--force", containerName)
		cleanupCancel()
		return result
	}

	streams := runClient(endpoint.Network, mainVolume, "", []byte("stdin-bytes"),
		[]string{remoteShell, "-c", "cat; printf stdout; printf stderr >&2; exit 7"})
	if streams.exitCode != 7 || string(streams.stdout) != "stdin-bytesstdout" || string(streams.stderr) != "stderr" {
		t.Fatalf("stream command = exit %d stdout %q stderr %q", streams.exitCode, streams.stdout, streams.stderr)
	}
	t.Log("exact static client preserved stdin, stdout, stderr, and nonzero status")

	tarBytes := integrationTar(t, "from-tar.txt", []byte("tar-bytes"))
	if result := runClient(endpoint.Network, mainVolume, "", tarBytes,
		[]string{remoteShell, "-c", "tar -xf -"}); result.exitCode != 0 {
		t.Fatalf("tar upload = exit %d: %s", result.exitCode, result.stderr)
	}
	t.Log("tar stream completed")
	write := runClient(endpoint.Network, mainVolume, "", nil,
		[]string{remoteShell, "-c", "printf bridge-state > bridge-state.txt; stat -c %i bridge-state.txt"})
	if write.exitCode != 0 {
		t.Fatalf("remote write = exit %d: %s", write.exitCode, write.stderr)
	}
	direct, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "cat /work/from-tar.txt; printf '\n'; cat /work/bridge-state.txt; printf '\n'; stat -c %i /work/bridge-state.txt"}})
	if err != nil || direct.ExitCode != 0 {
		t.Fatalf("direct sandbox read = %#v, %v", direct, err)
	}
	lines := strings.Split(strings.TrimSpace(direct.Stdout), "\n")
	if len(lines) != 3 || lines[0] != "tar-bytes" || lines[1] != "bridge-state" || strings.TrimSpace(string(write.stdout)) != lines[2] {
		t.Fatalf("shared filesystem evidence = remote inode %q direct %q", write.stdout, direct.Stdout)
	}
	t.Log("remote and evaluator paths share bytes and inode")

	wrongIdentity := writeWrongIdentity(t)
	wrongIdentityVolume := volumes.create(t, ctx, "wrong-id", wrongIdentity, endpoint.KnownHostsSourceFile, configSource)
	if result := runClient(endpoint.Network, wrongIdentityVolume, "", nil, []string{remoteShell, "-c", "true"}); result.exitCode == 0 {
		t.Fatal("wrong SSH identity connected")
	}
	wrongKnownHosts := writeWrongKnownHosts(t)
	wrongHostVolume := volumes.create(t, ctx, "wrong-host", endpoint.IdentitySourceFile, wrongKnownHosts, configSource)
	if result := runClient(endpoint.Network, wrongHostVolume, "", nil, []string{remoteShell, "-c", "true"}); result.exitCode == 0 || !strings.Contains(string(result.stderr), "host-key") {
		t.Fatalf("wrong host key = exit %d stderr %q", result.exitCode, result.stderr)
	}
	containerIP, err := sandbox.ContainerIPv4(ctx)
	if err != nil {
		t.Fatal(err)
	}
	passwordConfig := &ssh.ClientConfig{User: lockedUsername, Auth: []ssh.AuthMethod{ssh.Password("wrong")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second}
	if client, err := ssh.Dial("tcp", net.JoinHostPort(containerIP, "2222"), passwordConfig); err == nil {
		client.Close()
		t.Fatal("password authentication connected")
	}
	crossNetwork := runClient("", mainVolume, containerIP, nil, []string{remoteShell, "-c", "true"})
	if crossNetwork.exitCode == 0 {
		t.Fatal("container outside the task-scoped network reached the SSH listener")
	}
	t.Log("authentication, host-key, and cross-network negatives passed")

	background := runClient(endpoint.Network, mainVolume, "", nil,
		[]string{remoteShell, "-c", "(while [ ! -e release-after-stop ]; do sleep 1; done; printf leaked > delayed-after-stop) >/dev/null 2>&1 &"})
	if background.exitCode != 0 {
		t.Fatalf("launch delayed background descendant: exit %d stderr %q", background.exitCode, background.stderr)
	}
	bridge.mu.Lock()
	oldSigner, oldHostKey := bridge.active.clientSigner, bridge.active.hostKey
	bridge.mu.Unlock()
	artifactDir := filepath.Dir(endpoint.IdentitySourceFile)
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	bridgeStarted = false
	t.Log("bridge Stop positively revoked listener and credentials")
	if _, err := os.Lstat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bridge credentials remain: %v", err)
	}
	volumes.removeAll(t, ctx)
	restartedIP, err := sandbox.ContainerIPv4(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if connection, err := net.DialTimeout("tcp", net.JoinHostPort(restartedIP, "2222"), 300*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("SSH listener remains after Stop")
	}
	if err := probeSSH(ctx, restartedIP, oldSigner, oldHostKey); err == nil {
		t.Fatal("retained in-memory old signer connected after source and volume deletion")
	}
	release, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "touch /work/release-after-stop"}})
	if err != nil || release.ExitCode != 0 {
		t.Fatalf("release post-Stop descendant gate: %#v, %v", release, err)
	}
	time.Sleep(1500 * time.Millisecond)
	noDelayedWrite, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "test ! -e /work/delayed-after-stop"}})
	if err != nil || noDelayedWrite.ExitCode != 0 {
		t.Fatalf("SSH background descendant wrote after Stop: %#v, %v", noDelayedWrite, err)
	}
	if err := sandbox.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	assertEmptyBridgeInventory(t, ctx)
}

func requiredIntegrationHelper(t *testing.T, name string) string {
	t.Helper()
	path := os.Getenv(name)
	if path == "" {
		t.Fatalf("%s must name the statically built integration helper", name)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func writeIntegrationConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(lockedConfigContent()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type integrationVolumes struct {
	names []string
}

func newIntegrationVolumes(t *testing.T, _ context.Context) *integrationVolumes {
	t.Helper()
	volumes := &integrationVolumes{}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		volumes.removeAll(t, cleanupCtx)
	})
	return volumes
}

func (volumes *integrationVolumes) create(t *testing.T, ctx context.Context, suffix, identity, knownHosts, configuration string) string {
	t.Helper()
	name := "aries-m4-ssh-" + suffix
	result := runDocker(ctx, nil, "volume", "create",
		"--label", "aries.managed=true", "--label", "aries.milestone=m4", "--label", "aries.kind=ssh-material", name)
	if result.exitCode != 0 {
		t.Fatalf("create private SSH material volume: %s", result.stderr)
	}
	volumes.names = append(volumes.names, name)
	initializer := `set -eu; cp /tmp/aries-source-id /target/id_ed25519; cp /tmp/aries-source-known /target/known_hosts; cp /tmp/aries-source-config /target/config; chown 1000:1000 /target /target/id_ed25519 /target/known_hosts /target/config; chmod 0700 /target; chmod 0600 /target/id_ed25519 /target/known_hosts /target/config`
	result = runDetachedIntegrationScript(ctx, "aries-m4-volume-init-"+suffix, []string{
		"--user", "0:0",
		"--mount", "type=volume,src=" + name + ",dst=/target",
		"--mount", "type=bind,src=" + identity + ",dst=/tmp/aries-source-id,readonly",
		"--mount", "type=bind,src=" + knownHosts + ",dst=/tmp/aries-source-known,readonly",
		"--mount", "type=bind,src=" + configuration + ",dst=/tmp/aries-source-config,readonly",
	}, initializer)
	if result.exitCode != 0 {
		t.Fatalf("initialize private SSH material volume as root: %s", result.stderr)
	}
	verification := `test "$(id -u)" = "1000" && test "$(stat -c %u:%a /run/aries/ssh)" = "1000:700" && test "$(stat -c %u:%a /run/aries/ssh/id_ed25519)" = "1000:600" && test "$(stat -c %u:%a /run/aries/ssh/known_hosts)" = "1000:600" && test "$(stat -c %u:%a /tmp/openclaw-ssh-integration/config)" = "1000:600" && test -r /run/aries/ssh/id_ed25519 && test ! -w /run/aries/ssh/id_ed25519`
	result = runDetachedIntegrationScript(ctx, "aries-m4-volume-verify-"+suffix, []string{
		"--mount", "type=volume,src=" + name + ",dst=/run/aries/ssh,readonly",
		"--mount", "type=volume,src=" + name + ",dst=/tmp/openclaw-ssh-integration,readonly",
	}, verification)
	if result.exitCode != 0 {
		t.Fatalf("verify pinned OpenClaw default UID and private volume: %s", result.stderr)
	}
	return name
}

func runDetachedIntegrationScript(ctx context.Context, name string, options []string, script string) dockerResult {
	resultDir, err := os.MkdirTemp("", "aries-m4-detached-")
	if err != nil {
		return dockerResult{exitCode: -1, stderr: []byte(err.Error())}
	}
	defer os.RemoveAll(resultDir)
	wrapper := `set +e; mkdir -p /tmp/aries-result; /bin/sh -c "$1" >/tmp/aries-result/stdout 2>/tmp/aries-result/stderr; status=$?; printf '%s' "$status" >/tmp/aries-result/status; exit "$status"`
	args := []string{
		"container", "run", "--detach", "--name", name,
		"--label", "aries.managed=true", "--label", "aries.milestone=m4", "--label", "aries.kind=detached-fixture",
	}
	args = append(args, options...)
	args = append(args, pinnedOpenClawImage, "/bin/sh", "-c", wrapper, "--", script)
	launch := runDocker(ctx, nil, args...)
	if launch.exitCode != 0 {
		return launch
	}
	result := waitContainerResult(ctx, name, resultDir)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = runDocker(cleanupCtx, nil, "container", "rm", "--force", name)
	cancel()
	return result
}

func (volumes *integrationVolumes) removeAll(t *testing.T, ctx context.Context) {
	t.Helper()
	for len(volumes.names) > 0 {
		index := len(volumes.names) - 1
		name := volumes.names[index]
		result := runDocker(ctx, nil, "volume", "rm", name)
		if result.exitCode != 0 && !strings.Contains(string(result.stderr), "No such volume") {
			t.Errorf("remove SSH material volume %s: %s", name, result.stderr)
			return
		}
		volumes.names = volumes.names[:index]
	}
}

func ensureIntegrationImage(t *testing.T, ctx context.Context, image string) {
	t.Helper()
	if result := runDocker(ctx, nil, "image", "inspect", image); result.exitCode == 0 {
		return
	}
	if result := runDocker(ctx, nil, "pull", image); result.exitCode != 0 {
		t.Fatalf("pull digest-pinned integration image %s: %s", image, result.stderr)
	}
}

func writeWrongIdentity(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	content, err := marshalEd25519PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "wrong-id")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWrongKnownHosts(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("[%s]:%d %s", lockedHostName, lockedPort, ssh.MarshalAuthorizedKey(key))
	path := filepath.Join(t.TempDir(), "wrong-known-hosts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func integrationTar(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func runDocker(ctx context.Context, stdin []byte, args ...string) dockerResult {
	command := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return dockerResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}
}

func waitContainerResult(ctx context.Context, containerName, destination string) dockerResult {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	statusPath := filepath.Join(destination, "status")
	for {
		copyResult := runDocker(ctx, nil, "container", "cp", containerName+":/tmp/aries-result/status", statusPath)
		if copyResult.exitCode == 0 {
			content, err := os.ReadFile(statusPath)
			if err != nil {
				return dockerResult{exitCode: -1, stderr: []byte(err.Error())}
			}
			exitCode, err := strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil {
				return dockerResult{exitCode: -1, stderr: []byte("invalid detached command status")}
			}
			result := dockerResult{exitCode: exitCode}
			for name, target := range map[string]*[]byte{"stdout": &result.stdout, "stderr": &result.stderr} {
				path := filepath.Join(destination, name)
				if copied := runDocker(ctx, nil, "container", "cp", containerName+":/tmp/aries-result/"+name, path); copied.exitCode == 0 {
					*target, _ = os.ReadFile(path)
				}
			}
			return result
		}
		select {
		case <-ctx.Done():
			return dockerResult{exitCode: -1, stderr: []byte(ctx.Err().Error())}
		case <-ticker.C:
		}
	}
}

func assertEmptyBridgeInventory(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, args := range [][]string{
		{"container", "ls", "--all", "--quiet", "--filter", "label=aries.milestone=m4"},
		{"container", "ls", "--all", "--quiet", "--filter", "label=aries.milestone=m3"},
		{"network", "ls", "--quiet", "--filter", "label=aries.milestone=m3"},
		{"volume", "ls", "--quiet", "--filter", "label=aries.milestone=m4"},
	} {
		result := runDocker(ctx, nil, args...)
		if result.exitCode != 0 || strings.TrimSpace(string(result.stdout)) != "" {
			t.Fatalf("nonempty Docker inventory %v: exit %d stdout %q stderr %q", args[:2], result.exitCode, result.stdout, result.stderr)
		}
	}
}
