package agentplugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// A repository that asks for credentials fails at once: an askpass helper in
// the operator's environment could otherwise wait on a dialog.
func TestRunGitDoesNotCallAskpassHelpers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "asked")
	helper := filepath.Join(t.TempDir(), "askpass")
	script := "#!/bin/sh\ntouch '" + marker + "'\necho secret\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", helper)
	t.Setenv("SSH_ASKPASS", helper)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	if err := runGit(context.Background(), dir, "fetch", "--quiet", server.URL+"/private.git"); err == nil {
		t.Fatal("fetching a private repository without credentials succeeded")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the askpass helper ran (stat: %v)", err)
	}
}

// Every file under a checkout is staged, so a file that .gitignore hides from
// plain git status still makes the checkout unclean.
func TestFetchRejectsIgnoredFilesInACachedCheckout(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "--quiet")
	git(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	for name, content := range map[string]string{"plugin.json": "{}", ".gitignore": "generated/\n"} {
		if err := os.WriteFile(filepath.Join(origin, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, origin, "add", ".")
	git(t, origin, "commit", "--quiet", "-m", "plugin")
	revision := git(t, origin, "rev-parse", "HEAD")

	root := filepath.Join(t.TempDir(), "demo@"+revision)
	if err := Fetch(context.Background(), root, "file://"+origin, revision); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(context.Background(), root, "file://"+origin, revision); err != nil {
		t.Fatalf("a clean cached checkout must be accepted: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generated", "mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Fetch(context.Background(), root, "file://"+origin, revision)
	if err == nil || !strings.Contains(err.Error(), "ignored files") {
		t.Fatalf("an ignored file must make the checkout unclean, got %v", err)
	}
}
