package toolathlon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryDetectsSourcelessBytecode(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("runtime inventory regression requires python3")
	}
	root := t.TempDir()
	for _, name := range []string{".venv/bin/python", "bin/uv"} {
		writeFile(t, root, name, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inventory := func(name string) string {
		t.Helper()
		manifest := filepath.Join(t.TempDir(), name)
		cmd := exec.Command(python, "-I", "-", root, manifest)
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin"), "HOME="+root)
		cmd.Stdin = bytes.NewReader(inventoryProgram)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inventory: %v: %s", err, output)
		}
		return manifest
	}
	before := inventory("before")
	// Normal interpreter caches may change while the MCP servers run.
	writeFile(t, root, ".venv/lib/python3.12/site-packages/__pycache__/module.cpython-312.pyc", "cache")
	if err := compareRuntimeManifests(before, inventory("cached")); err != nil {
		t.Fatalf("normal cache changed inventory: %v", err)
	}
	// Compile a real sourceless startup module, not just a filename fixture.
	const relative = ".venv/lib/python3.12/site-packages/sitecustomize.pyc"
	source := filepath.Join(t.TempDir(), "sitecustomize.py")
	if err := os.WriteFile(source, []byte("raise RuntimeError('planted startup code')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", "-c", "import py_compile,sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)", source, filepath.Join(root, relative))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile fixture: %v: %s", err, output)
	}
	if err := compareRuntimeManifests(before, inventory("planted")); err == nil || !strings.Contains(err.Error(), relative) {
		t.Fatalf("sourceless bytecode was not detected: %v", err)
	}
}

func TestCalendarCredentialStaging(t *testing.T) {
	for _, missing := range []string{"", "gcp-oauth.keys.json", "google_credentials.json"} {
		t.Run("missing="+missing, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			for _, file := range []string{"gcp-oauth.keys.json", "google_credentials.json"} {
				if file != missing {
					writeFile(t, root, "configs/"+file, "fixture:"+file)
				}
			}
			cmd := exec.Command("/bin/sh", "-ceu", calendarCredentialsScript, "aries-toolathlon-calendar", root)
			cmd.Env = append(os.Environ(), "HOME="+home)
			output, err := cmd.CombinedOutput()
			if missing != "" {
				if err == nil {
					t.Fatal("missing Calendar credential accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("stage: %v: %s", err, output)
			}
			for destination, source := range map[string]string{"gcp-oauth.keys.json": "gcp-oauth.keys.json", "credentials.json": "google_credentials.json"} {
				path := filepath.Join(home, ".calendar-mcp", destination)
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "fixture:"+source {
					t.Fatalf("wrong Calendar credential at %s: %v", destination, err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("credential permissions: %v, %v", info, err)
				}
			}
		})
	}
}
