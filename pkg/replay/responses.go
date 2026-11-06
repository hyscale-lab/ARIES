package replay

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

var ErrExecResponseTooLarge = errors.New("Exec response exceeds replay audit line bound")

// LiveAuditError checks the finalized gRPC audit for output that could not fit
// in one replay record. It fails closed if the private audit is not intact.
func LiveAuditError(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open live bridge audit directory: %w", err)
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("stat live bridge audit: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != privateTimingFileMode || info.Size() < 0 || info.Size() > maxTimingAuditBytes {
		return errors.New("live bridge audit is not a bounded private file")
	}
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open live bridge audit: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return errors.New("live bridge audit changed before reading")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxTimingAuditBytes+1))
	var overflow bool
	for {
		var record struct {
			ReplayError string `json:"replay_error"`
		}
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode live bridge audit: %w", err)
		}
		if record.ReplayError == "response-debug-too-large" {
			overflow = true
		}
	}
	closed, err := file.Stat()
	if err != nil || !os.SameFile(info, closed) || closed.Size() != info.Size() {
		return errors.New("live bridge audit changed while reading")
	}
	if overflow {
		return ErrExecResponseTooLarge
	}
	return nil
}

// ReadResponses compares finalized private audit hashes after bridge revocation.
func ReadResponses(path string, plan Plan, logger *logrus.Logger) core.ReplayResponses {
	unavailable := func(reason string) core.ReplayResponses {
		return core.ReplayResponses{Status: "unavailable", Reason: reason}
	}
	if path == "" {
		return unavailable("replay bridge audit is missing")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return unavailable("replay bridge audit is unavailable")
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != privateTimingFileMode || info.Size() < 0 || info.Size() > maxTimingAuditBytes {
		return unavailable("replay bridge audit is not a bounded private file")
	}
	file, err := root.Open(name)
	if err != nil {
		return unavailable("replay bridge audit is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != info.Mode().Perm() || opened.Size() != info.Size() {
		return unavailable("replay bridge audit changed before reading")
	}
	type auditResponse struct {
		SourceSequence uint64 `json:"replay_source_sequence"`
		OperationClass string `json:"operation_class"`
		SHA256         string `json:"response_sha256"`
		Debug          bool   `json:"response_debug"`
		Stdout         string `json:"response_stdout"`
		Stderr         string `json:"response_stderr"`
		StdoutBase64   []byte `json:"response_stdout_base64"`
		StderrBase64   []byte `json:"response_stderr_base64"`
	}
	records := make(map[uint64]auditResponse, len(plan.Calls))
	decoder := json.NewDecoder(io.LimitReader(file, maxTimingAuditBytes+1))
	for {
		var record auditResponse
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			return unavailable("replay bridge audit has an invalid record")
		}
		if record.SourceSequence == 0 || records[record.SourceSequence].SourceSequence != 0 || len(records) >= len(plan.Calls) {
			return unavailable("replay bridge audit has extra or duplicate calls")
		}
		records[record.SourceSequence] = record
	}
	closed, err := file.Stat()
	if err != nil || !os.SameFile(info, closed) || closed.Mode().Perm() != info.Mode().Perm() || closed.Size() != info.Size() {
		return unavailable("replay bridge audit changed while reading")
	}
	responses := core.ReplayResponses{Status: "available"}
	for _, call := range plan.Calls {
		record, exists := records[call.Sequence]
		if !exists {
			responses.Status, responses.Reason = "unavailable", "replay bridge audit has missing calls"
			continue
		}
		decoded, digestErr := hex.DecodeString(record.SHA256)
		if digestErr != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != record.SHA256 || record.OperationClass != operationClass(call.Operation) {
			responses.Status, responses.Reason = "unavailable", "replay bridge audit has invalid response evidence"
			continue
		}
		status := "matched"
		if record.SHA256 != call.ResponseSHA256 {
			status = "drifted"
			entry := logger.WithFields(logrus.Fields{"source_sequence": call.Sequence, "operation": call.Operation, "expected_sha256": call.ResponseSHA256, "actual_sha256": record.SHA256})
			entry.Warn("replay response hash drift")
			if call.Operation == "Exec" {
				if !call.ResponseDebug {
					entry.Warn("source output unavailable")
				} else {
					entry.WithFields(logrus.Fields{"expected_stdout": responseText(call.ResponseStdout), "expected_stderr": responseText(call.ResponseStderr)}).Info("source Exec output")
				}
				if record.Debug {
					stdout, stderr := []byte(record.Stdout), []byte(record.Stderr)
					if record.StdoutBase64 != nil {
						stdout = record.StdoutBase64
					}
					if record.StderrBase64 != nil {
						stderr = record.StderrBase64
					}
					entry.WithFields(logrus.Fields{"actual_stdout": responseText(stdout), "actual_stderr": responseText(stderr)}).Info("replay Exec output")
				}
			}
		}
		responses.Calls = append(responses.Calls, core.ReplayCallResponse{SourceSequence: call.Sequence, Status: status, ExpectedSHA256: call.ResponseSHA256, ActualSHA256: record.SHA256})
	}
	if len(records) != len(plan.Calls) {
		responses.Status, responses.Reason = "unavailable", "replay bridge audit has missing or unknown calls"
	}
	if responses.Status != "available" {
		responses.Calls = nil
	}
	return responses
}

func operationClass(operation string) string {
	switch operation {
	case "Exec":
		return "exec"
	case "Stat":
		return "stat"
	case "ReadFile", "ReadLines":
		return "read"
	case "WriteFile":
		return "write"
	}
	return ""
}

func responseText(content []byte) string {
	if utf8.Valid(content) {
		return string(content)
	}
	return "base64: " + base64.StdEncoding.EncodeToString(content)
}
