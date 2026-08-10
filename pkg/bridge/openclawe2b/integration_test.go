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
		stdout, stderr, code := startProcess(t, httpClient, fixtures[index], []string{"-c", "printf task-stdout; printf task-stderr >&2; exit 7"})
		if stdout != "task-stdout" || stderr != "task-stderr" || code != 7 {
			t.Fatalf("task %d stream = stdout %q stderr %q code %d", index, stdout, stderr, code)
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
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/stat", `{"path":"/workspace/a/source.bin"}`)
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/make-dir", `{"path":"/workspace/b/c"}`)
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/move", `{"source":"/workspace/a/source.bin","destination":"/workspace/b/c/moved.bin"}`)
	if got := readRaw(t, httpClient, fixtures[0], "/workspace/b/c/moved.bin"); !bytes.Equal(got, []byte{0, 1, 255}) {
		t.Fatalf("raw file bytes = %v", got)
	}
	postJSON(t, httpClient, fixtures[0], "/v1/filesystem/remove", `{"path":"/workspace/b"}`)

	for _, signal := range []string{"SIGNAL_SIGTERM", "SIGNAL_SIGKILL"} {
		pid, done := startSleepingProcess(t, httpClient, fixtures[0])
		postJSON(t, httpClient, fixtures[0], "/v1/process/send-signal", fmt.Sprintf(`{"process":{"pid":%d},"signal":%q}`, pid, signal))
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not end attached Process.Start", signal)
		}
	}

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
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
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

func startProcess(t *testing.T, client *http.Client, fixture realTaskFixture, args []string) (string, string, int) {
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
	exitCode := -1
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var event processEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event.Data != nil {
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
			exitCode = event.Event.End.ExitCode
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), exitCode
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
