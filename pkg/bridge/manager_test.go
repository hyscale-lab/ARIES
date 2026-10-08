package bridge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"google.golang.org/grpc"
)

type lifecycleRuntime struct {
	closes   int
	closeErr error
	deployment.Runtime
	running              bool
	stops, downloads     int
	content              []byte
	downloadErr, stopErr error
}

func (r *lifecycleRuntime) Close() error { r.closes++; return r.closeErr }

func (r *lifecycleRuntime) Running(context.Context, string) (bool, error) { return r.running, nil }
func (r *lifecycleRuntime) Stop(context.Context, string) error            { r.stops++; return r.stopErr }
func (r *lifecycleRuntime) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	r.downloads++
	if r.downloadErr != nil {
		return nil, deployment.FileInfo{}, r.downloadErr
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: "tool-calls.jsonl", Size: int64(len(r.content)), Mode: 0600, Typeflag: tar.TypeReg})
	_, _ = w.Write(r.content)
	_ = w.Close()
	return io.NopCloser(&b), deployment.FileInfo{Size: int64(len(r.content)), Mode: 0600}, nil
}

type revokeClient struct {
	v1.BridgeControlClient
	assignment *v1.Assignment
	err        error
	calls      int
}

func (c *revokeClient) RevokeAssignment(context.Context, *v1.AssignmentRequest, ...grpc.CallOption) (*v1.Assignment, error) {
	c.calls++
	return c.assignment, c.err
}
func fixtureManager(t *testing.T) (*Manager, *lifecycleRuntime, *revokeClient) {
	t.Helper()
	r := &lifecycleRuntime{running: true, content: []byte("{\"command\":\"true\"}\n")}
	sum := sha256.Sum256(r.content)
	d := core.BridgeTarget{Version: 1, RuntimeID: "immutable"}
	c := &revokeClient{assignment: &v1.Assignment{InstanceId: "instance", AssignmentId: "assignment", Target: control.TargetToProto(d), State: v1.State_REVOKED, Artifacts: []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete", Size: int64(len(r.content)), Sha256: hex.EncodeToString(sum[:])}}}}
	m := &Manager{options: Options{Runtime: r}, runtimeID: "runtime", instance: "instance", assignment: "assignment", descriptor: d, client: c, assigned: true, local: t.TempDir(), remote: "evidence/task/bridge"}
	return m, r, c
}
func TestStopRequiresRevocationBeforeCollectAndRemove(t *testing.T) {
	for _, state := range []v1.State{v1.State_ASSIGNING, v1.State_READY, v1.State_REVOKING} {
		t.Run(state.String(), func(t *testing.T) {
			m, r, c := fixtureManager(t)
			c.assignment.State = state
			if err := m.Stop(context.Background()); err == nil {
				t.Fatal("unconfirmed revocation succeeded")
			}
			if r.stops != 0 || r.downloads != 0 {
				t.Fatal("runtime removed before revocation")
			}
		})
	}
	m, r, c := fixtureManager(t)
	c.err = errors.New("control connection lost")
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("control failure treated as confirmed revocation while child is running")
	}
	if r.stops != 0 {
		t.Fatal("cleanup erased collection ownership")
	}
}

func TestStopReportsNativeCleanupFailure(t *testing.T) {
	m, r, c := fixtureManager(t)
	c.assignment.State = v1.State_REVOKING
	c.assignment.CleanupErrors = []*v1.CleanupError{{Stage: "revocation", Message: "native handler cleanup failed"}}
	err := m.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "REVOKING") || !strings.Contains(err.Error(), "revocation: native handler cleanup failed") {
		t.Fatalf("missing cleanup diagnostic: %v", err)
	}
	if r.stops != 0 || r.downloads != 0 {
		t.Fatal("unconfirmed cleanup released evidence ownership")
	}
}
func TestStopCollectsEvidenceThenRemovesAndErasesCredentials(t *testing.T) {
	m, r, c := fixtureManager(t)
	key := filepath.Join(m.local, "id_ed25519")
	if err := os.WriteFile(key, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	m.secretFiles = []string{key}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.stops != 1 || r.downloads != 1 {
		t.Fatal("incomplete cleanup")
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("key remains")
	}
	b, err := os.ReadFile(filepath.Join(m.local, "tool-calls.jsonl"))
	if err != nil || !bytes.Equal(b, r.content) {
		t.Fatal("evidence differs")
	}
	if err = m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 || r.stops != 1 || r.downloads != 1 {
		t.Fatal("terminal cleanup repeated")
	}
}
func TestEvidenceFailureRetainsRuntimeForRetry(t *testing.T) {
	m, r, c := fixtureManager(t)
	r.downloadErr = errors.New("temporary archive failure")
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("missing evidence accepted")
	}
	if r.stops != 0 {
		t.Fatal("runtime removed before collection")
	}
	r.downloadErr = nil
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 || r.stops != 1 {
		t.Fatal("retry repeated revocation or missed cleanup")
	}
}

func TestRemovalRetryUsesAlreadyVerifiedLocalEvidence(t *testing.T) {
	m, runtime, client := fixtureManager(t)
	runtime.stopErr = errors.New("runtime termination observed; metadata removal conflict")
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("removal conflict ignored")
	}
	runtime.stopErr = nil
	runtime.downloadErr = errors.New("terminated runtime no longer accepts archive reads")
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.downloads != 1 || runtime.stops != 2 || client.calls != 1 {
		t.Fatal("removal retry discarded finalized evidence or repeated revocation")
	}
}
func TestStopRejectsManifestCorruptionAndCrossAssignment(t *testing.T) {
	for _, change := range []func(*v1.Assignment){func(a *v1.Assignment) { a.InstanceId = "other" }, func(a *v1.Assignment) { a.Target.RuntimeId = "other" }, func(a *v1.Assignment) { a.Artifacts = nil }, func(a *v1.Assignment) { a.Artifacts[0].Sha256 = "bad" }, func(a *v1.Assignment) { a.Artifacts[0].Name = "../private" }} {
		m, r, c := fixtureManager(t)
		change(c.assignment)
		if err := m.Stop(context.Background()); err == nil {
			t.Fatal("invalid response accepted")
		}
		if r.stops != 0 {
			t.Fatal("invalid response discarded runtime")
		}
	}
}
func TestPartialStartBeforeAdmissionRemovesOwnedRuntime(t *testing.T) {
	m, r, _ := fixtureManager(t)
	m.assigned = false
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.stops != 1 || r.downloads != 0 {
		t.Fatal("partial allocation cleanup incomplete")
	}
}

func TestExposedChildCrashReportsMissingEvidenceAfterDisposal(t *testing.T) {
	m, r, c := fixtureManager(t)
	m.exposed = true
	r.running = false
	c.err = errors.New("control connection lost")
	key := filepath.Join(m.local, "id_ed25519")
	if err := os.WriteFile(key, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	m.secretFiles = []string{key}
	for range 2 {
		if err := m.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "finalized evidence") {
			t.Fatalf("missing evidence after crash not reported: %v", err)
		}
	}
	if r.stops != 1 || r.downloads != 0 {
		t.Fatal("crashed runtime cleanup did not retain uncertainty")
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crashed runtime credential remains")
	}
}

func TestUnexposedChildExitAllowsOwnedCleanup(t *testing.T) {
	m, r, c := fixtureManager(t)
	r.running = false
	c.err = errors.New("child exited before access was exposed")
	for range 2 {
		if err := m.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if r.stops != 1 || r.downloads != 0 || r.closes != 1 || c.calls != 1 {
		t.Fatalf("unexpected failed-start cleanup: %+v", r)
	}
}

func TestAbsentEvidenceAllowedOnlyBeforeReady(t *testing.T) {
	for _, exposed := range []bool{false, true} {
		m, r, c := fixtureManager(t)
		m.exposed = exposed
		c.assignment.Artifacts = []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "absent"}}
		err := m.Stop(context.Background())
		if exposed && err == nil {
			t.Fatal("exposed bridge missing evidence accepted")
		}
		if !exposed && (err != nil || r.stops != 1) {
			t.Fatalf("failed admission cleanup: %v", err)
		}
	}
}

func TestClientHelperStagingMatchesNativeHarnessPermissions(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "installed-helper"), filepath.Join(root, "aries-ssh-client")
	content := []byte("matched-native-helper")
	if err := os.WriteFile(source, content, 0755); err != nil {
		t.Fatal(err)
	}
	if err := stageClientHelper(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0555 {
		t.Fatalf("harness requires exact0555 regular source: %v %v", info, err)
	}
	staged, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(staged, content) {
		t.Fatal("helper contents changed")
	}
	before, err := os.Stat(source)
	if err != nil || before.Mode().Perm() != 0755 {
		t.Fatal("installed helper changed")
	}
	if err := stageClientHelper(source, destination); err == nil {
		t.Fatal("existing task helper overwritten")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := stageClientHelper(link, filepath.Join(root, "linked-copy")); err == nil {
		t.Fatal("symlink helper accepted")
	}
}

func TestRuntimeCloseRetriesWithoutReplayingRevocation(t *testing.T) {
	m, r, c := fixtureManager(t)
	r.closeErr = errors.New("transport close failed")
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("close failure hidden")
	}
	if r.closes != 1 || r.stops != 1 || c.calls != 1 {
		t.Fatal("unexpected first cleanup calls")
	}
	r.closeErr = nil
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.closes != 2 || r.stops != 1 || c.calls != 1 {
		t.Fatalf("close=%d stop=%d revoke=%d", r.closes, r.stops, c.calls)
	}
}
