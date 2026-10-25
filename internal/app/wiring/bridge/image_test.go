package bridge

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestKubernetesImageAppendsTheBuildCommit(t *testing.T) {
	previous := Revision
	t.Cleanup(func() { Revision = previous })

	Revision = "0123456789ab"
	if image, err := KubernetesImage("aries-bridge:v1"); err != nil || image != "aries-bridge:v1-0123456789ab" {
		t.Fatalf("image = %q, %v", image, err)
	}
	Revision = "0123456789ab-dirty"
	if image, err := KubernetesImage("aries-bridge:v1"); err != nil || image != "aries-bridge:v1-0123456789ab-dirty" {
		t.Fatalf("dirty image = %q, %v", image, err)
	}
	Revision = "main"
	if _, err := KubernetesImage("aries-bridge:v1"); err == nil {
		t.Fatal("a revision that is not a commit was accepted")
	}
}

func TestBuildRevisionReadsGosStamp(t *testing.T) {
	stamp := func(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Settings: settings}, true }
	}
	commit := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}
	if got := buildRevision(stamp(commit, debug.BuildSetting{Key: "vcs.modified", Value: "false"})); got != "0123456789ab" {
		t.Fatalf("clean = %q", got)
	}
	if got := buildRevision(stamp(commit, debug.BuildSetting{Key: "vcs.modified", Value: "true"})); got != "0123456789ab-dirty" {
		t.Fatalf("modified = %q", got)
	}
	if got := buildRevision(stamp()); got != "" {
		t.Fatalf("unstamped = %q", got)
	}
	if got := buildRevision(func() (*debug.BuildInfo, bool) { return nil, false }); got != "" {
		t.Fatalf("no build info = %q", got)
	}
}

func TestKubernetesImageExplainsAnUnstampedBinary(t *testing.T) {
	previous := Revision
	t.Cleanup(func() { Revision = previous })
	Revision = ""
	// A test binary carries no VCS stamp.
	if _, err := KubernetesImage("aries-bridge:v1"); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Fatalf("unstamped = %v", err)
	}
}
