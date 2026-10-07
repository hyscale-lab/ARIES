package hermes

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runSpanDump runs spanDumpScript with the host's python3 against a store the
// test builds, so the script's exits and byte limit are checked for real.
func runSpanDump(t *testing.T, spans []string, limit int) (string, string, int) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	store := filepath.Join(t.TempDir(), "hermes_otel_live.db")
	if len(spans) > 0 {
		setup := `import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT, data TEXT)")
db.execute("INSERT INTO events (kind, data) VALUES ('metric', '{}')")
for data in sys.argv[2:]:
    db.execute("INSERT INTO events (kind, data) VALUES ('span', ?)", (data,))
db.commit()
`
		if output, err := exec.Command(python, append([]string{"-c", setup, store}, spans...)...).CombinedOutput(); err != nil {
			t.Fatalf("build store: %v\n%s", err, output)
		}
	}
	command := exec.Command(python, "-c", spanDumpScript, store, strconv.Itoa(limit))
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	code := 0
	if err := command.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func TestSpanDumpScriptWritesSpansInOrder(t *testing.T) {
	stdout, stderr, code := runSpanDump(t, []string{`{"name":"a"}`, `{"name":"b"}`}, 1<<20)
	if code != 0 || stdout != "{\"name\":\"a\"}\n{\"name\":\"b\"}\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestSpanDumpScriptReportsMissingStore(t *testing.T) {
	if stdout, _, code := runSpanDump(t, nil, 1<<20); code != spanStoreMissing || stdout != "" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestSpanDumpScriptStopsAtWholeLineUnderLimit(t *testing.T) {
	stdout, stderr, code := runSpanDump(t, []string{`{"name":"a"}`, `{"name":"b"}`}, len("{\"name\":\"a\"}\n")+5)
	if code != spansTruncated || stdout != "{\"name\":\"a\"}\n" || !strings.Contains(stderr, "kept 1 of 2 spans") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
