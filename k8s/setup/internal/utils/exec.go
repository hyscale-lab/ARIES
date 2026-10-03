package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// failureTail is how many trailing output lines a failed command's error
// carries. Enough to show the actual failure; the full output is in the log.
const failureTail = 25

// ExecShellCmd runs one command line under bash with pipefail and returns its
// trimmed stdout. Output is appended to the log, not shown.
//
// The format is only passed through fmt.Sprintf when args are given, so a
// literal command containing '%' (awk, date) needs no escaping. Every value
// interpolated into a command line must go through Quote: these lines are
// parsed by a shell, and an unquoted value is a place for input to become
// syntax.
func ExecShellCmd(format string, args ...any) (string, error) {
	return run(render(format, args), false, true)
}

// ExecShellCmdStreaming is ExecShellCmd with output also written to the
// terminal as it arrives, for steps that run for minutes.
func ExecShellCmdStreaming(format string, args ...any) error {
	_, err := run(render(format, args), true, true)
	return err
}

// ExecShellCmdSecret runs a command whose stdout is a credential. The command
// line is logged; the output is not. Reading admin.conf through ExecShellCmd
// would copy a cluster-admin client certificate into a plain log file.
func ExecShellCmdSecret(format string, args ...any) (string, error) {
	return run(render(format, args), false, false)
}

func render(format string, args []any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

func run(command string, stream, logOutput bool) (string, error) {
	appendLog("$ %s\n", command)
	var stdout, stderr, combined bytes.Buffer

	outWriters := []io.Writer{&stdout, &combined}
	errWriters := []io.Writer{&stderr, &combined}
	if logOutput {
		outWriters = append(outWriters, logWriter{})
		errWriters = append(errWriters, logWriter{})
	}
	if stream {
		outWriters = append(outWriters, lockedStdout{})
		errWriters = append(errWriters, lockedStdout{})
	}

	cmd := exec.Command("bash", "-o", "pipefail", "-c", command)
	// No stdin: nothing here is interactive, and create_cluster runs commands
	// from parallel goroutines that must not contend for the terminal.
	cmd.Stdout = io.MultiWriter(outWriters...)
	cmd.Stderr = io.MultiWriter(errWriters...)
	err := cmd.Run()
	if err != nil {
		detail := combined.String()
		if !logOutput {
			// Only stderr is safe to surface for a secret-bearing command.
			detail = stderr.String()
		}
		return strings.TrimSpace(stdout.String()), &CommandError{Command: command, Err: err, Tail: tail(detail, failureTail)}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// CommandError is a failed command with the end of its output attached.
type CommandError struct {
	Command string
	Err     error
	Tail    string
}

func (e *CommandError) Error() string {
	if e.Tail == "" {
		return fmt.Sprintf("%s: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("%s: %v\n%s", e.Command, e.Err, e.Tail)
}

func (e *CommandError) Unwrap() error { return e.Err }

// ExitCode reports a failed command's exit status, or -1 if it did not exit.
func ExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func tail(text string, lines int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	parts := strings.Split(text, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return "    " + strings.Join(parts, "\n    ")
}

// Quote renders s as a single POSIX shell word. It is the only safe way to put
// a value into a line passed to ExecShellCmd, and it composes: quoting an
// already-quoted remote command for ssh yields a line the local shell reduces
// to exactly the remote command.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteAll quotes each argument and joins them with spaces.
func QuoteAll(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = Quote(arg)
	}
	return strings.Join(quoted, " ")
}

// WriteFile writes a configuration file directly rather than through a shell
// heredoc, so its content is never subject to shell expansion.
func WriteFile(path, content string, mode os.FileMode) error {
	appendLog("# write %s (%d bytes, mode %o)\n", path, len(content), mode)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, mode)
}

// RequireRoot fails unless the process runs as root. On-node subcommands
// install packages and write under /etc, and are run through sudo so a
// password prompt never stalls an unattended step half-way through.
func RequireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this subcommand must run as root (sudo %s ...)", os.Args[0])
	}
	return nil
}
