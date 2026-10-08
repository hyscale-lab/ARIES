package bridge

import (
	"context"
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
	return evidenceArchive(), deployment.FileInfo{Size: int64(len(evidenceContent())), Mode: 0600}, nil
}

type releaseClient struct {
	v1.BridgeControlClient
	access *v1.SandboxAccess
	err    error
	calls  int
}

func (c *releaseClient) ReleaseSandbox(context.Context, *v1.SandboxRequest, ...grpc.CallOption) (*v1.SandboxAccess, error) {
	c.calls++
	return c.access, c.err
}
func fixtureSession(t *testing.T) (*Session, *lifecycleRuntime, *releaseClient) {
	t.Helper()
	r := &lifecycleRuntime{running: true}
	c := &releaseClient{access: &v1.SandboxAccess{SandboxId: "sandbox", State: v1.State_RELEASED, Artifacts: evidenceManifest()}}
	service := &Service{options: Options{Runtime: r}, runtimeID: "runtime", client: c, state: "running"}
	s := &Session{service: service, descriptor: core.BridgeTarget{SandboxID: "sandbox"}, registered: true, local: t.TempDir(), remote: "remote"}
	service.sessions = []*Session{s}
	return s, r, c
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
