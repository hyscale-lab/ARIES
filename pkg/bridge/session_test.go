package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"google.golang.org/grpc"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type lifecycleRuntime struct {
	deployment.Runtime
	running                        bool
	stops, downloads, closes       int
	downloadErr, stopErr, closeErr error
}

func (r *lifecycleRuntime) Running(context.Context, string) (bool, error) { return r.running, nil }
func (r *lifecycleRuntime) Stop(context.Context, string) error            { r.stops++; return r.stopErr }
func (r *lifecycleRuntime) Close() error                                  { r.closes++; return r.closeErr }
func (r *lifecycleRuntime) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	r.downloads++
	if r.downloadErr != nil {
		return nil, deployment.FileInfo{}, r.downloadErr
	}
	return evidenceArchive("tool-calls.jsonl"), deployment.FileInfo{Size: int64(len(evidenceContent())), Mode: 0600}, nil
}

type releaseClient struct {
	v1.BridgeControlClient
	access        *v1.SandboxAccess
	err           error
	completionErr error
	calls         int
}

func (c *releaseClient) ReleaseSandbox(context.Context, *v1.SandboxRequest, ...grpc.CallOption) (*v1.SandboxAccess, error) {
	c.calls++
	return c.access, c.err
}

const fixtureCompletedAt = "2026-11-05T12:00:00Z"

func (c *releaseClient) CompleteSandbox(_ context.Context, r *v1.SandboxRequest, _ ...grpc.CallOption) (*v1.Completion, error) {
	if c.completionErr != nil {
		return nil, c.completionErr
	}
	return &v1.Completion{SandboxId: r.SandboxId, CompletedAt: fixtureCompletedAt}, nil
}
func fixtureSession(t *testing.T) (*Session, *lifecycleRuntime, *releaseClient) {
	t.Helper()
	r := &lifecycleRuntime{running: true}
	c := &releaseClient{access: &v1.SandboxAccess{SandboxId: "sandbox", State: v1.State_RELEASED, Artifacts: evidenceManifest("tool-calls.jsonl")}}
	service := &Service{options: Options{Runtime: r}, runtimeID: "runtime", client: c, state: "running", transport: "ssh"}
	s := &Session{service: service, descriptor: core.BridgeTarget{SandboxID: "sandbox"}, registered: true, local: t.TempDir(), remote: "remote"}
	service.sessions = []*Session{s}
	return s, r, c
}
func TestSessionFinalizesReplaySourceOnlyWithCompletionAndMatchingImage(t *testing.T) {
	const logicalID = "catalog-task"
	const image = "task-image:tag"
	const benchmarkRevision = "pinned-revision"
	const sourceSchemaVersion = 1
	for _, test := range []struct {
		name            string
		complete        bool
		signalErr       bool
		image           string
		corrupt         bool
		downloadFailure bool
		ready           bool
	}{
		{name: "complete", complete: true, image: image, ready: true},
		{name: "missing completion", image: image},
		{name: "completion signal failed", complete: true, signalErr: true, image: image},
		{name: "changed occurrence image", complete: true, image: "other-image:tag"},
		{name: "corrupt manifest", complete: true, image: image, corrupt: true},
		{name: "partial collection", complete: true, image: image, downloadFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, runtime, client := fixtureSession(t)
			s.service.transport = "grpc"
			s.exposed = true
			s.service.options.BenchmarkType = "terminalbench2"
			s.service.options.BenchmarkRevisions = map[string]string{"revision": benchmarkRevision}
			s.descriptor.TaskID = "catalog-task-001"
			s.descriptor.RuntimeID = "target-runtime"
			s.service.runtimeName = "bridge-runtime-name"
			s.service.options.Launch.RuntimeBackend = "docker"
			s.RecordSource(logicalID, image)
			s.RecordTask(test.image)
			if err := s.record(); err != nil {
				t.Fatal(err)
			}
			if test.complete {
				if test.signalErr {
					client.completionErr = errors.New("completion unavailable")
				}
				if err := s.CompleteHarness(context.Background()); (err != nil) != test.signalErr {
					t.Fatalf("CompleteHarness() error = %v", err)
				}
			}
			if test.corrupt {
				client.access.Artifacts[0].Sha256 = "invalid"
			}
			if test.downloadFailure {
				runtime.downloadErr = errors.New("incomplete transfer")
			}
			err := s.Stop(context.Background())
			if (err != nil) != (test.corrupt || test.downloadFailure) {
				t.Fatalf("Stop() error = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(s.local, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata struct {
				SchemaVersion      int               `json:"schema_version"`
				BenchmarkType      string            `json:"benchmark_type"`
				TaskID             string            `json:"task_id"`
				Image              string            `json:"image"`
				BenchmarkRevisions map[string]string `json:"benchmark_revisions"`
				HarnessCompletedAt string            `json:"harness_completed_at"`
				ToolCallsBytes     *int64            `json:"tool_calls_bytes"`
				ToolCallsSHA256    string            `json:"tool_calls_sha256"`
				ReplayReady        bool              `json:"replay_ready"`
				RuntimeID          string            `json:"RuntimeID"`
				RuntimeName        string            `json:"RuntimeName"`
				SandboxID          string            `json:"SandboxID"`
				TargetRuntimeID    string            `json:"TargetRuntimeID"`
			}
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.SchemaVersion != sourceSchemaVersion || metadata.TaskID != logicalID || metadata.Image != image || metadata.BenchmarkType != "terminalbench2" || metadata.BenchmarkRevisions["revision"] != benchmarkRevision || metadata.ReplayReady != test.ready || metadata.RuntimeID != "runtime" || metadata.RuntimeName != "bridge-runtime-name" || metadata.SandboxID != "sandbox" || metadata.TargetRuntimeID != "target-runtime" {
				t.Fatalf("runtime metadata = %+v", metadata)
			}
			if test.complete && !test.signalErr && !test.corrupt && !test.downloadFailure && metadata.HarnessCompletedAt != fixtureCompletedAt {
				t.Fatalf("completion receipt = %q", metadata.HarnessCompletedAt)
			}
			if test.signalErr && (metadata.HarnessCompletedAt != "" || client.calls != 1) {
				t.Fatalf("failed completion retained marker or skipped release: %+v, releases = %d", metadata, client.calls)
			}
			if test.corrupt || test.downloadFailure {
				if client.calls != 1 || metadata.ToolCallsBytes != nil || metadata.ToolCallsSHA256 != "" {
					t.Fatalf("invalid evidence was bound or release skipped: %+v, releases = %d", metadata, client.calls)
				}
				return
			}
			if metadata.ToolCallsBytes == nil || *metadata.ToolCallsBytes != int64(len(evidenceContent())) || metadata.ToolCallsSHA256 != evidenceManifest("tool-calls.jsonl")[0].Sha256 {
				t.Fatalf("collected log binding = %+v", metadata)
			}
			info, err := os.Stat(filepath.Join(s.local, "runtime.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private runtime metadata = %v, %v", info, err)
			}
		})
	}
}

func TestSessionOversizedExecAuditCannotClaimReplayReady(t *testing.T) {
	const image = "task-image:tag"
	s, _, _ := fixtureSession(t)
	s.service.transport = "grpc"
	s.service.options.BenchmarkType = "terminalbench2"
	s.service.options.BenchmarkRevisions = map[string]string{"revision": "pinned-revision"}
	s.exposed, s.released, s.collected = true, true, true
	s.metadata.HarnessCompletedAt = fixtureCompletedAt
	s.RecordSource("catalog-task", image)
	s.RecordTask(image)
	content := []byte("{\"replay_error\":\"response-debug-too-large\"}\n")
	path := filepath.Join(s.local, "tool-calls.jsonl")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	s.manifest = []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete", Size: int64(len(content)), Sha256: hex.EncodeToString(sum[:])}}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("confirmed release rejected bounded marker: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.local, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		ReplayReady bool `json:"replay_ready"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.ReplayReady {
		t.Fatalf("oversized response claimed replay readiness: %+v, %v", metadata, err)
	}
}

func TestFinalMetadataFailureStillRemovesHelperAfterRelease(t *testing.T) {
	s, runtime, client := fixtureSession(t)
	s.service.transport = "grpc"
	s.service.options.BenchmarkType = "terminalbench2"
	s.service.options.BenchmarkRevisions = map[string]string{"revision": "pinned-revision"}
	s.RecordSource("catalog-task", "task-image:tag")
	s.RecordTask("task-image:tag")
	s.exposed = true
	if err := s.record(); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteHarness(context.Background()); err != nil {
		t.Fatal(err)
	}
	initial := filepath.Join(s.local, "runtime.json")
	unready := filepath.Join(s.local, "runtime.unready.json")
	if err := os.Rename(initial, unready); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(initial, 0700); err != nil {
		t.Fatal(err)
	}
	s.helper = filepath.Join(s.local, "staged-helper")
	if err := os.WriteFile(s.helper, []byte("helper"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Stop(context.Background()); err == nil {
			t.Fatal("invalid final metadata accepted")
		}
		if _, err := os.Stat(s.helper); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staged helper remains after release: %v", err)
		}
	}
	if client.calls != 1 || runtime.downloads != 1 || !s.released || !s.collected {
		t.Fatalf("release calls = %d, downloads = %d, released = %t, collected = %t", client.calls, runtime.downloads, s.released, s.collected)
	}
	data, err := os.ReadFile(unready)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		ReplayReady bool `json:"replay_ready"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.ReplayReady {
		t.Fatalf("source became replay-ready: %v, %v", metadata, err)
	}
}

func TestSessionStopRequiresReleaseBeforeEvidence(t *testing.T) {
	for _, state := range []v1.State{v1.State_REGISTERING, v1.State_READY, v1.State_RELEASING} {
		s, r, c := fixtureSession(t)
		c.access.State = state
		if err := s.Stop(context.Background()); err == nil {
			t.Fatal(state)
		}
		if r.stops != 0 || r.downloads != 0 {
			t.Fatal("unconfirmed cleanup discarded ownership")
		}
	}
}
func TestEvidenceFailureRetriesWithoutAffectingService(t *testing.T) {
	s, r, c := fixtureSession(t)
	r.downloadErr = errors.New("archive failure")
	if err := s.Stop(context.Background()); err == nil {
		t.Fatal("lost evidence accepted")
	}
	r.downloadErr = nil
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.stops != 0 || r.closes != 0 || r.downloads != 2 || c.calls != 1 {
		t.Fatal(r, c.calls)
	}
	if err := s.service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.stops != 1 || r.closes != 1 {
		t.Fatal(r)
	}
}
func TestSessionRejectsCorruptManifestOrSandboxIdentity(t *testing.T) {
	for _, change := range []func(*v1.SandboxAccess){func(a *v1.SandboxAccess) { a.SandboxId = "other" }, func(a *v1.SandboxAccess) { a.Artifacts = nil }, func(a *v1.SandboxAccess) { a.Artifacts[0].Sha256 = "wrong" }, func(a *v1.SandboxAccess) { a.Artifacts[0].Name = "../private" }} {
		s, r, c := fixtureSession(t)
		s.exposed = true
		change(c.access)
		if err := s.Stop(context.Background()); err == nil {
			t.Fatal("invalid evidence accepted")
		}
		if r.stops != 0 {
			t.Fatal("session removed shared runtime")
		}
	}
}
func TestCrashReportsEvidenceFailureButOnlyServiceRemovesRuntime(t *testing.T) {
	s, r, c := fixtureSession(t)
	s.exposed = true
	r.running = false
	c.err = errors.New("disconnected")
	for range 2 {
		if err := s.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "finalized evidence") {
			t.Fatal(err)
		}
	}
	if r.stops != 0 {
		t.Fatal("session removed runtime")
	}
	if err := s.service.Stop(context.Background()); err == nil {
		t.Fatal("run lost evidence failure")
	}
	if r.stops != 1 || r.closes != 1 {
		t.Fatal(r)
	}
}
func TestUnexposedCrashAndReleasedBeforeRegistrationCleanlyStop(t *testing.T) {
	for _, crash := range []bool{false, true} {
		s, r, c := fixtureSession(t)
		if crash {
			r.running = false
			c.err = errors.New("disconnected")
		} else {
			c.access.Artifacts = nil
		}
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.downloads != 0 || r.stops != 0 {
			t.Fatal(r)
		}
	}
}
func TestServiceRemovalAndCloseRetry(t *testing.T) {
	s, r, c := fixtureSession(t)
	r.stopErr = errors.New("remove failed")
	if err := s.service.Stop(context.Background()); err == nil {
		t.Fatal("remove error hidden")
	}
	r.stopErr = nil
	r.closeErr = errors.New("close failed")
	if err := s.service.Stop(context.Background()); err == nil {
		t.Fatal("close error hidden")
	}
	r.closeErr = nil
	if err := s.service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.downloads != 1 || r.stops != 2 || r.closes != 2 || c.calls != 1 {
		t.Fatal(r, c.calls)
	}
}
func TestClientHelperStagingAndSessionCleanup(t *testing.T) {
	s, _, _ := fixtureSession(t)
	source := filepath.Join(t.TempDir(), "helper")
	s.helper = filepath.Join(s.local, "client")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := stageClientHelper(source, s.helper); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.helper)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal(info, err)
	}
	if err := stageClientHelper(source, s.helper); err == nil {
		t.Fatal("overwrote existing helper")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(source, link)
	if err := stageClientHelper(link, s.helper); err == nil {
		t.Fatal("symlink source accepted")
	}
}
