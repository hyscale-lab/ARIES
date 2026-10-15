package hermes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture stand-ins for the two pinned Hermes files, reduced to the anchor
// lines the seam edits. The integration test proves the anchors against the
// real image; this pins the script's own behaviour.
const (
	seamBaseFixture      = "class BaseEnvironment(ABC):\n    def _before_execute(self) -> None:\n        pass\n"
	seamFileToolsFixture = "def _get_file_ops(task_id):\n    file_ops = ShellFileOperations(terminal_env)\n    return file_ops\n"
)

func runSeam(t *testing.T, root string) (int, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	command := exec.Command(python, "seam.py", root)
	output, err := command.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(output)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(output)
}

func writeSeamFixtures(t *testing.T, base, fileTools string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools", "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"tools/environments/base.py": base, "tools/file_tools.py": fileTools} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readSeamFixtures(t *testing.T, root string) (string, string) {
	t.Helper()
	base, err := os.ReadFile(filepath.Join(root, "tools/environments/base.py"))
	if err != nil {
		t.Fatal(err)
	}
	fileTools, err := os.ReadFile(filepath.Join(root, "tools/file_tools.py"))
	if err != nil {
		t.Fatal(err)
	}
	return string(base), string(fileTools)
}

func TestSeamAddsTheFileOperationsHookOnce(t *testing.T) {
	root := writeSeamFixtures(t, seamBaseFixture, seamFileToolsFixture)
	if code, output := runSeam(t, root); code != 0 {
		t.Fatalf("seam exit %d: %s", code, output)
	}
	base, fileTools := readSeamFixtures(t, root)
	if strings.Count(base, "def get_file_operations(self):") != 1 || !strings.Contains(base, "        return None\n") {
		t.Fatalf("base.py was not given the default hook:\n%s", base)
	}
	if !strings.Contains(fileTools, "file_ops = terminal_env.get_file_operations() or ShellFileOperations(terminal_env)") {
		t.Fatalf("file_tools.py does not ask the environment:\n%s", fileTools)
	}

	// A restarted container runs the wrapper again.
	if code, output := runSeam(t, root); code != 0 {
		t.Fatalf("second run exit %d: %s", code, output)
	}
	again, againTools := readSeamFixtures(t, root)
	if again != base || againTools != fileTools {
		t.Fatal("a second run changed the patched files")
	}
}

// A moved pin must stop the container, and must not leave one file patched and
// the other not.
func TestSeamRefusesAMissingAnchorWithoutWriting(t *testing.T) {
	root := writeSeamFixtures(t, seamBaseFixture, "file_ops = something_else()\n")
	code, output := runSeam(t, root)
	if code == 0 || !strings.Contains(output, "anchor not found") {
		t.Fatalf("seam exit %d: %s", code, output)
	}
	base, _ := readSeamFixtures(t, root)
	if base != seamBaseFixture {
		t.Fatalf("base.py was written despite the failure:\n%s", base)
	}
}
