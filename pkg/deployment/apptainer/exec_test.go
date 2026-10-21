package apptainer

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestExecShellReportsEveryExitInItsTrailer runs the wrapper with the host's
// /bin/sh, as apptainer runs it in an instance.
func TestExecShellReportsEveryExitInItsTrailer(t *testing.T) {
	environment := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(environment, []byte("GREETING='hello'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		dir     string
		command []string
		want    int
	}{
		"command exit":    {t.TempDir(), []string{"/bin/sh", "-c", `test "$GREETING" = hello && exit 3`}, 3},
		"missing workdir": {filepath.Join(t.TempDir(), "missing"), []string{"/bin/true"}, 126},
	} {
		var stderr bytes.Buffer
		wrapper := exec.Command("/bin/sh", append([]string{"-c", execShell, "aries-exec", "tok", c.dir, environment}, c.command...)...)
		wrapper.Stderr = &stderr
		var exit *exec.ExitError
		if err := wrapper.Run(); !errors.As(err, &exit) || exit.ExitCode() != c.want {
			t.Errorf("%s: wrapper = %v", name, err)
		}
		if want := "\x1eARIES_EXEC_EXIT_tok=" + strconv.Itoa(c.want); !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: stderr %q has no trailer %q", name, stderr.String(), want)
		}
	}
}
