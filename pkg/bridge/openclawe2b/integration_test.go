//go:build integration

package openclawe2b_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawe2b"
	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const e2bFixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

type processEvent struct {
	Event struct {
		Start *struct {
			PID int `json:"pid"`
		} `json:"start,omitempty"`
		Data *struct {
			Stdout string `json:"stdout,omitempty"`
			Stderr string `json:"stderr,omitempty"`
		} `json:"data,omitempty"`
		End *struct {
			ExitCode int     `json:"exitCode"`
			Error    *string `json:"error"`
		} `json:"end,omitempty"`
	} `json:"event"`
}

type realTaskFixture struct {
	id       string
	manager  *dockersandbox.Manager
	sandbox  *dockersandbox.Sandbox
	grant    *openclawe2b.Grant
	endpoint core.ToolEndpoint
	token    string
}

func TestRealDockerCentralBridgeMultiplexesAndCleansTwoSandboxes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-e2b-integration/1"))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required for integration tests: %v", err)
	}
	ensureImage(t, ctx, api, e2bFixtureImage)

	server := openclawe2b.New()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	fixtures := make([]realTaskFixture, 2)
	var wg sync.WaitGroup
	for index := range fixtures {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			id := fmt.Sprintf("e2b-real-%d", index)
			manager, startErr := dockersandbox.New(dockersandbox.Options{OutputDir: t.TempDir(), CleanupTimeout: 20 * time.Second, Logger: logrus.New()})
			if startErr != nil {
				t.Errorf("New sandbox %d: %v", index, startErr)
				return
			}
			live, startErr := manager.Start(ctx, core.SandboxRequest{RunID: "e2b-real", TaskID: id, Environment: core.Environment{Image: e2bFixtureImage, Workdir: "/workspace", CPU: .5, MemoryMB: 32, StorageMB: 64}})
			if startErr != nil {
				t.Errorf("Start sandbox %d: %v", index, startErr)
				return
			}
			sandbox := live.(*dockersandbox.Sandbox)
			grant := server.NewGrant(t.TempDir())
			endpoint, startErr := grant.Start(ctx, sandbox)
			if startErr != nil {
				_ = manager.Stop(context.Background(), sandbox)
				t.Errorf("Start grant %d: %v", index, startErr)
				return
			}
			tokenBytes, readErr := os.ReadFile(endpoint.AccessTokenSourceFile)
			if readErr != nil {
				t.Errorf("read token %d: %v", index, readErr)
			}
			fixtures[index] = realTaskFixture{id: id, manager: manager, sandbox: sandbox, grant: grant, endpoint: endpoint, token: string(tokenBytes)}
		}(index)
	}
	wg.Wait()
	for index := range fixtures {
		if fixtures[index].sandbox == nil {
			t.FailNow()
		}
		fixture := &fixtures[index]
		t.Cleanup(func() {
			_ = fixture.grant.Stop(context.Background())
			_ = fixture.manager.Stop(context.Background(), fixture.sandbox)
		})
	}

	addressA, _ := url.Parse(fixtures[0].endpoint.Address)
	addressB, _ := url.Parse(fixtures[1].endpoint.Address)
	if addressA.Port() == "" || addressA.Port() != addressB.Port() || addressA.Hostname() == addressB.Hostname() {
		t.Fatalf("task endpoints do not share one listener through distinct gateways: %q / %q", addressA, addressB)
	}
	if fixtures[0].endpoint.SandboxID == fixtures[1].endpoint.SandboxID || fixtures[0].token == fixtures[1].token {
		t.Fatal("task grants reused sandbox ID or token")
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}
	for index := range fixtures {
		other := fixtures[1-index]
		own := execForEvidence(t, ctx, fixtures[index].sandbox, core.Command{Path: "/bin/wget", Args: []string{"-T", "3", "-S", "-O", "/dev/null", "--header", "E2b-Sandbox-Id: " + fixtures[index].endpoint.SandboxID, "--header", "X-Access-Token: " + fixtures[index].token, fixtures[index].endpoint.Address + "/unknown"}})
		if !strings.Contains(own.Stderr, "501 Not Implemented") {
			t.Fatalf("task %d did not reach its gateway listener: %#v", index, own)
		}
		cross := execForEvidence(t, ctx, fixtures[index].sandbox, core.Command{Path: "/bin/wget", Args: []string{"-T", "2", "-S", "-O", "/dev/null", other.endpoint.Address + "/unknown"}})
		if strings.Contains(cross.Stderr, "501 Not Implemented") {
			t.Fatalf("task %d reached the other task gateway: %#v", index, cross)
		}
	}
	for index := range fixtures {
		stdout, stderr, code, pid := startProcess(t, httpClient, fixtures[index], []string{"-c", "printf \"task\\000stdout\"; printf \"task\\000stderr\" >&2; exit 7"})
		wantStdout := []byte{'t', 'a', 's', 'k', 0, 's', 't', 'd', 'o', 'u', 't'}
		if !bytes.Equal(stdout, wantStdout) || !bytes.Equal(stderr, []byte{'t', 'a', 's', 'k', 0, 's', 't', 'd', 'e', 'r', 'r'}) || code != 7 {
			t.Fatalf("task %d stream = stdout %q stderr %q code %d pid %d", index, stdout, stderr, code, pid)
		}
	}
	wrong := bridgeRequest(t, fixtures[1].endpoint.Address+"/v1/files?path=/workspace/none", http.MethodGet, fixtures[0], nil)
	if response, err := httpClient.Do(wrong); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("cross-gateway credentials status = %d", response.StatusCode)
		}
	}

	writeRaw(t, httpClient, fixtures[0], "/workspace/a/source.bin", []byte{0, 1, 255})
	writeRaw(t, httpClient, fixtures[0], "/workspace/a/zero.bin", nil)
	if got := readRaw(t, httpClient, fixtures[0], "/workspace/a/zero.bin"); len(got) != 0 {
		t.Fatalf("zero-byte read = %v", got)
	}
	writeRaw(t, httpClient, fixtures[0], "/workspace/a/source.bin", nil)
	if got := readRaw(t, httpClient, fixtures[0], "/workspace/a/source.bin"); len(got) != 0 {
		t.Fatalf("zero-byte overwrite = %v", got)
	}
	writeRaw(t, httpClient, fixtures[0], "/workspace/a/source.bin", []byte{0, 1, 255})
	stat := postJSONResult(t, httpClient, fixtures[0], "/v1/filesystem/stat", `{"path":"/workspace/a/source.bin"}`, http.StatusOK)
	if !bytes.Contains(stat, []byte(`"type":"file"`)) || !bytes.Contains(stat, []byte(`"size":3`)) {
		t.Fatalf("stat metadata = %s", stat)
	}
	listing := postJSONResult(t, httpClient, fixtures[0], "/v1/filesystem/list-dir", `{"path":"/workspace/a"}`, http.StatusOK)
	if !bytes.Contains(listing, []byte(`"name":"source.bin"`)) || !bytes.Contains(listing, []byte(`"name":"zero.bin"`)) {
		t.Fatalf("depth-one listing = %s", listing)
	}
	root := postJSONResult(t, httpClient, fixtures[0], "/v1/filesystem/remove", `{"path":"/"}`, http.StatusBadRequest)
	if !bytes.Contains(root, []byte("root")) {
		t.Fatalf("root removal response = %s", root)
	}
	execForEvidence(t, ctx, fixtures[0].sandbox, core.Command{Path: "/bin/ln", Args: []string{"-s", "/workspace/a/source.bin", "/workspace/a/link.bin"}})
	linkStat := postJSONResult(t, httpClient, fixtures[0], "/v1/filesystem/stat", `{"path":"/workspace/a/link.bin"}`, http.StatusOK)
	if !bytes.Contains(linkStat, []byte(`"type":"symlink"`)) || !bytes.Contains(linkStat, []byte(`"linkTarget":"/workspace/a/source.bin"`)) {
		t.Fatalf("symlink stat = %s", linkStat)
	}
	if status := rawReadStatus(t, httpClient, fixtures[0], "/workspace/a/link.bin"); status == http.StatusOK {
		t.Fatal("raw symlink read unexpectedly succeeded")
	}
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/make-dir", `{"path":"/workspace/b/c"}`)
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/move", `{"source":"/workspace/a/source.bin","destination":"/workspace/b/c/moved.bin"}`)
	if got := readRaw(t, httpClient, fixtures[0], "/workspace/b/c/moved.bin"); !bytes.Equal(got, []byte{0, 1, 255}) {
		t.Fatalf("raw file bytes = %v", got)
	}
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/remove", `{"path":"/workspace/b"}`)

	for _, signal := range []string{"SIGNAL_SIGTERM", "SIGNAL_SIGKILL"} {
		pid, done := startSleepingProcess(t, httpClient, fixtures[0])
		crossSignal := postJSONResult(t, httpClient, fixtures[1], "/v1/process/send-signal", fmt.Sprintf(`{"process":{"pid":%d},"signal":%q}`, pid, signal), http.StatusNotFound)
		if !bytes.Contains(crossSignal, []byte("not active")) {
			t.Fatalf("cross-sandbox signal = %s", crossSignal)
		}
		postJSON(t, httpClient, fixtures[0], "/v1/process/send-signal", fmt.Sprintf(`{"process":{"pid":%d},"signal":%q}`, pid, signal))
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not end attached Process.Start", signal)
		}
		stale := postJSONResult(t, httpClient, fixtures[0], "/v1/process/send-signal", fmt.Sprintf(`{"process":{"pid":%d},"signal":%q}`, pid, signal), http.StatusNotFound)
		if !bytes.Contains(stale, []byte("not active")) {
			t.Fatalf("stale signal = %s", stale)
		}
	}
	testClientCancellation(t, fixtures[0])

	_, revokedProcessDone := startSleepingProcess(t, httpClient, fixtures[0])
	tokenPathA := fixtures[0].endpoint.AccessTokenSourceFile
	if err := fixtures[0].grant.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-revokedProcessDone:
	case <-time.After(15 * time.Second):
		t.Fatal("grant revocation returned without draining its attached process")
	}
	if _, err := os.Stat(tokenPathA); !os.IsNotExist(err) {
		t.Fatalf("revoked token remains: %v", err)
	}
	if response, err := httpClient.Do(bridgeRequest(t, fixtures[0].endpoint.Address+"/v1/files?path=/workspace/x", http.MethodGet, fixtures[0], nil)); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("revoked grant status = %d", response.StatusCode)
		}
	}
	writeRaw(t, httpClient, fixtures[1], "/workspace/still-live", []byte("task-b"))
	if got := readRaw(t, httpClient, fixtures[1], "/workspace/still-live"); string(got) != "task-b" {
		t.Fatalf("task B after task A cleanup = %q", got)
	}

	_, shutdownDone := startSleepingProcess(t, httpClient, fixtures[1])
	listenerAddress := fixtures[1].endpoint.Address
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(15 * time.Second):
		t.Fatal("server shutdown did not drain task B process")
	}
	if response, err := httpClient.Do(bridgeRequest(t, listenerAddress+"/v1/files?path=/workspace/still-live", http.MethodGet, fixtures[1], nil)); err == nil {
		response.Body.Close()
		t.Fatal("central listener remains reachable after shutdown")
	}

	for index := range fixtures {
		if err := fixtures[index].grant.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		containerID, networkName := fixtures[index].sandbox.ContainerID(), fixtures[index].sandbox.NetworkName()
		if err := fixtures[index].manager.Stop(ctx, fixtures[index].sandbox); err != nil {
			t.Fatal(err)
		}
		if _, err := api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("task %d container remains: %v", index, err)
		}
		if _, err := api.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("task %d network remains: %v", index, err)
		}
	}
}

func execForEvidence(t *testing.T, ctx context.Context, sandbox *dockersandbox.Sandbox, command core.Command) core.CommandResult {
	t.Helper()
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func bridgeRequest(t *testing.T, target, method string, fixture realTaskFixture, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("E2b-Sandbox-Id", fixture.endpoint.SandboxID)
	request.Header.Set("X-Access-Token", fixture.token)
	return request
}

func startProcess(t *testing.T, client *http.Client, fixture realTaskFixture, args []string) ([]byte, []byte, int, int) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"process": map[string]any{"cmd": "/bin/sh", "args": args, "cwd": "/workspace", "envs": map[string]string{}}})
	response, err := client.Do(bridgeRequest(t, fixture.endpoint.Address+"/v1/process/start", http.MethodPost, fixture, bytes.NewReader(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("Process.Start = %d: %s", response.StatusCode, content)
	}
	var stdout, stderr bytes.Buffer
	exitCode, pid, phase := -1, 0, 0
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var event processEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event.Start != nil {
			if phase != 0 || event.Event.Start.PID <= 1 {
				t.Fatalf("invalid start ordering: %#v", event)
			}
			pid, phase = event.Event.Start.PID, 1
		}
		if event.Event.Data != nil {
			if phase != 1 {
				t.Fatalf("data outside start/end: %#v", event)
			}
			decoded, err := base64.StdEncoding.DecodeString(event.Event.Data.Stdout)
			if err != nil {
				t.Fatal(err)
			}
			stdout.Write(decoded)
			decoded, err = base64.StdEncoding.DecodeString(event.Event.Data.Stderr)
			if err != nil {
				t.Fatal(err)
			}
			stderr.Write(decoded)
		}
		if event.Event.End != nil {
			if phase != 1 {
				t.Fatalf("end outside active stream: %#v", event)
			}
			exitCode, phase = event.Event.End.ExitCode, 2
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if phase != 2 {
		t.Fatalf("stream ended in phase %d", phase)
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode, pid
}

func startSleepingProcess(t *testing.T, client *http.Client, fixture realTaskFixture) (int, <-chan struct{}) {
	t.Helper()
	payload := strings.NewReader(`{"process":{"cmd":"/bin/sh","args":["-c","trap 'exit 0' TERM; while :; do sleep 1; done"],"cwd":"/workspace","envs":{}}}`)
	response, err := client.Do(bridgeRequest(t, fixture.endpoint.Address+"/v1/process/start", http.MethodPost, fixture, payload))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var event processEvent
	if err := json.Unmarshal(line, &event); err != nil || event.Event.Start == nil {
		t.Fatalf("start event = %s, %v", line, err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); response.Body.Close(); close(done) }()
	return event.Event.Start.PID, done
}

func writeRaw(t *testing.T, client *http.Client, fixture realTaskFixture, path string, content []byte) {
	t.Helper()
	request := bridgeRequest(t, fixture.endpoint.Address+"/v1/files?path="+url.QueryEscape(path), http.MethodPost, fixture, bytes.NewReader(content))
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("write %s = %d: %s", path, response.StatusCode, body)
	}
}

func readRaw(t *testing.T, client *http.Client, fixture realTaskFixture, path string) []byte {
	t.Helper()
	response, err := client.Do(bridgeRequest(t, fixture.endpoint.Address+"/v1/files?path="+url.QueryEscape(path), http.MethodGet, fixture, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("read %s = %d %v: %s", path, response.StatusCode, err, content)
	}
	return content
}

func postJSON(t *testing.T, client *http.Client, fixture realTaskFixture, route, payload string) {
	t.Helper()
	request := bridgeRequest(t, fixture.endpoint.Address+route, http.MethodPost, fixture, strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s = %d: %s", route, response.StatusCode, body)
	}
}

func ensureImage(t *testing.T, ctx context.Context, api *client.Client, image string) {
	t.Helper()
	if _, err := api.ImageInspect(ctx, image); err == nil {
		return
	}
	pull, err := api.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func postJSONResult(t *testing.T, client *http.Client, fixture realTaskFixture, route, payload string, wantStatus int) []byte {
	t.Helper()
	request := bridgeRequest(t, fixture.endpoint.Address+route, http.MethodPost, fixture, strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("POST %s = %d, want %d: %s", route, response.StatusCode, wantStatus, body)
	}
	return body
}

func rawReadStatus(t *testing.T, client *http.Client, fixture realTaskFixture, path string) int {
	t.Helper()
	response, err := client.Do(bridgeRequest(t, fixture.endpoint.Address+"/v1/files?path="+url.QueryEscape(path), http.MethodGet, fixture, nil))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func testClientCancellation(t *testing.T, fixture realTaskFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	payload := strings.NewReader(`{"process":{"cmd":"/bin/sh","args":["-c","while :; do sleep 1; done"],"cwd":"/workspace","envs":{}}}`)
	request := bridgeRequest(t, fixture.endpoint.Address+"/v1/process/start", http.MethodPost, fixture, payload).WithContext(ctx)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var event processEvent
	if err := json.Unmarshal(line, &event); err != nil || event.Event.Start == nil {
		t.Fatalf("cancellation start = %s, %v", line, err)
	}
	pid := event.Event.Start.PID
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, execErr := fixture.sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "test ! -d /proc/$1", "aries-cancel-check", strconv.Itoa(pid)}})
		if execErr == nil && result.ExitCode == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("canceled process %d remains", pid)
}
