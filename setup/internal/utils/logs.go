// Package utils holds the output and command helpers shared by aries-setup's
// subcommands.
package utils

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Everything aries-setup prints goes through one mutex. create_cluster joins
// workers from parallel goroutines, and unsynchronised writes interleave
// mid-line.
var (
	outputMu sync.Mutex
	logFile  *os.File
	colour   = isTerminal(os.Stdout)
)

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// OpenLog starts appending every executed command and its output to path.
// Terminal output stays short; the log is where apt, kubeadm and kubectl
// output goes, and where to look when a step fails.
func OpenLog(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}
	outputMu.Lock()
	logFile = file
	outputMu.Unlock()
	appendLog("\n===== aries-setup started %s =====\n", time.Now().Format(time.RFC3339))
	return nil
}

// CloseLog flushes and closes the log opened by OpenLog.
func CloseLog() {
	outputMu.Lock()
	defer outputMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

func appendLog(format string, args ...any) {
	outputMu.Lock()
	defer outputMu.Unlock()
	if logFile != nil {
		fmt.Fprintf(logFile, format, args...)
	}
}

// logWriter adapts the shared log to io.Writer for command output.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	outputMu.Lock()
	defer outputMu.Unlock()
	if logFile == nil {
		return len(p), nil
	}
	return logFile.Write(p)
}

// lockedStdout serialises streamed command output against the printers below.
type lockedStdout struct{}

func (lockedStdout) Write(p []byte) (int, error) {
	outputMu.Lock()
	defer outputMu.Unlock()
	return os.Stdout.Write(p)
}

var _ io.Writer = logWriter{}

func printLine(stream *os.File, code, tag, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	outputMu.Lock()
	defer outputMu.Unlock()
	if colour {
		fmt.Fprintf(stream, "\033[%sm%s\033[0m %s\n", code, tag, message)
	} else {
		fmt.Fprintf(stream, "%s %s\n", tag, message)
	}
	if logFile != nil {
		fmt.Fprintf(logFile, "%s %s\n", tag, message)
	}
}

// WaitPrintf announces a step that is about to run.
func WaitPrintf(format string, args ...any) { printLine(os.Stdout, "1;34", "==>", format, args...) }

// InfoPrintf prints detail under the current step.
func InfoPrintf(format string, args ...any) { printLine(os.Stdout, "0", "   ", format, args...) }

// SuccessPrintf reports a completed step or result.
func SuccessPrintf(format string, args ...any) { printLine(os.Stdout, "1;32", " ok", format, args...) }

// WarnPrintf reports something the operator should know but that does not stop the run.
func WarnPrintf(format string, args ...any) { printLine(os.Stderr, "1;33", "warn", format, args...) }

// ErrorPrintf reports a failure.
func ErrorPrintf(format string, args ...any) { printLine(os.Stderr, "1;31", "error", format, args...) }
