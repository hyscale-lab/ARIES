package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every value in a command line goes through Quote, so its one job is that
// the shell reads back exactly the original string — including the inputs
// that would otherwise become syntax.
func TestQuoteRoundTripsThroughBash(t *testing.T) {
	inputs := []string{
		"", "plain", "has space", "it's", `double "quoted"`, "$(touch /tmp/pwned)",
		"`id`", "a;b", "a|b", "a&b", "line\nbreak", "glob*?[x]", "~user", "-leading-dash", `back\slash`,
	}
	for _, input := range inputs {
		out, err := exec.Command("bash", "-c", "printf %s "+Quote(input)).Output()
		if err != nil {
			t.Fatalf("Quote(%q) produced an invalid line: %v", input, err)
		}
		if string(out) != input {
			t.Errorf("Quote(%q) read back as %q", input, out)
		}
	}
}

// Safe words stay readable in logs; anything else is quoted.
func TestQuoteLeavesSafeWordsBare(t *testing.T) {
	for _, word := range []string{"kubeadm", "10.0.0.1:6443", "user@host.example", "sha256:abc", "unix:///run/containerd/containerd.sock"} {
		if Quote(word) != word {
			t.Errorf("Quote(%q) = %q, want it unquoted", word, Quote(word))
		}
	}
	if Quote("a b") == "a b" {
		t.Error("a word containing a space must be quoted")
	}
}

// ssh passes its command to a second shell. Quoting an already-quoted line
// must reduce, after one shell, to exactly that line.
func TestQuoteComposesAcrossTwoShells(t *testing.T) {
	inner := "printf %s " + Quote("it's $(dangerous)")
	out, err := exec.Command("bash", "-c", "bash -c "+Quote(inner)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "it's $(dangerous)" {
		t.Errorf("two-shell round trip = %q", out)
	}
}

func TestExecShellCmdUsesPipefail(t *testing.T) {
	if _, err := ExecShellCmd("false | true"); err == nil {
		t.Error("a failure early in a pipeline must fail the command; `curl | gpg` depends on it")
	}
}

// Without args the format is not passed through Sprintf, so awk and date
// lines need no %-escaping.
func TestExecShellCmdLeavesPercentAloneWithoutArgs(t *testing.T) {
	out, err := ExecShellCmd("printf '%s' hello")
	if err != nil || out != "hello" {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestFailedCommandCarriesOutputTail(t *testing.T) {
	_, err := ExecShellCmd("echo the-real-reason >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "the-real-reason") {
		t.Fatalf("error = %v, want it to include the command's stderr", err)
	}
	if ExitCode(err) != 3 {
		t.Errorf("ExitCode = %d, want 3", ExitCode(err))
	}
}

// fetch_kubeconfig reads admin.conf through ExecShellCmdSecret. Its output is
// a cluster-admin client certificate and must never reach the log file.
func TestSecretOutputStaysOutOfTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.log")
	if err := OpenLog(path); err != nil {
		t.Fatal(err)
	}
	// The output is assembled at run time, so it never appears literally in
	// the logged command line and any occurrence in the log is a leak.
	command := "printf 'client-key-data-%s' SECRET"
	out, err := ExecShellCmdSecret(command)
	CloseLog()
	if err != nil || out != "client-key-data-SECRET" {
		t.Fatalf("got %q, %v", out, err)
	}
	logged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "client-key-data-SECRET") {
		t.Error("secret command output was written to the log")
	}
	if !strings.Contains(string(logged), "$ "+command) {
		t.Error("the command line itself should still be logged")
	}
}

// The control case: ordinary command output is logged, so the test above is
// proving something rather than passing because nothing is ever logged.
func TestOrdinaryOutputIsLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.log")
	if err := OpenLog(path); err != nil {
		t.Fatal(err)
	}
	_, err := ExecShellCmd("printf 'visible-%s' OUTPUT")
	CloseLog()
	if err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(path)
	if !strings.Contains(string(logged), "visible-OUTPUT") {
		t.Error("ordinary output should reach the log")
	}
}
