package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

var ErrExecResponseTooLarge = errors.New("Exec response exceeds replay audit line bound")

// LiveAuditError checks the finalized gRPC audit for output that could not fit
// in one replay record. It fails closed if the private audit is not intact.
func LiveAuditError(path string) error {
	var overflow bool
	err := readPrivateAudit(path, true, func(decoder *json.Decoder) error {
		for {
			var record struct {
				ReplayError string `json:"replay_error"`
			}
			if err := decoder.Decode(&record); err != nil {
				if err == io.EOF {
					return nil
				}
				return fmt.Errorf("decode live bridge audit: %w", err)
			}
			if record.ReplayError == "response-debug-too-large" {
				overflow = true
			}
		}
	})
	if err != nil {
		return err
	}
	if overflow {
		return ErrExecResponseTooLarge
	}
	return nil
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
	Status         string `json:"status"`
	GRPCCode       string `json:"grpc_code"`
	ReplayError    string `json:"replay_error"`
	ExitCode       *int32 `json:"exit_code"`
	Truncated      bool   `json:"truncated"`
	StdinBytes     *int64 `json:"stdin_bytes"`
}

func (record auditResponse) output() ([]byte, []byte) {
	stdout, stderr := []byte(record.Stdout), []byte(record.Stderr)
	if record.StdoutBase64 != nil {
		stdout = record.StdoutBase64
	}
	if record.StderrBase64 != nil {
		stderr = record.StderrBase64
	}
	return stdout, stderr
}

// ReadResponses compares finalized private audit evidence after bridge revocation.
func ReadResponses(path string, plan Plan, logger *logrus.Logger) core.ReplayResponses {
	unavailable := func(reason string) core.ReplayResponses {
		return core.ReplayResponses{Status: "unavailable", Reason: reason}
	}
	if path == "" {
		return unavailable("replay bridge audit is missing")
	}
	count := plan.DispatchCount()
	var records map[uint64]auditResponse
	err := readPrivateAudit(path, false, func(decoder *json.Decoder) error {
		records = make(map[uint64]auditResponse, count)
		for {
			var record auditResponse
			if err := decoder.Decode(&record); err != nil {
				if err == io.EOF {
					return nil
				}
				return errors.New("replay bridge audit has an invalid record")
			}
			if record.SourceSequence == 0 || records[record.SourceSequence].SourceSequence != 0 || len(records) >= count {
				return errors.New("replay bridge audit has extra or duplicate calls")
			}
			records[record.SourceSequence] = record
		}
	})
	if err != nil {
		return unavailable(err.Error())
	}
	responses := core.ReplayResponses{Status: "available"}
	for _, call := range plan.Calls {
		if call.SourceRejection != nil {
			continue
		}
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
		if plan.OriginType == OriginHarbor {
			status := harborResponseStatus(call, record)
			if status == "" {
				responses.Status, responses.Reason = "unavailable", "replay bridge audit has invalid Harbor response evidence"
				continue
			}
			responses.Calls = append(responses.Calls, core.ReplayCallResponse{SourceSequence: call.Sequence, Status: status})
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
					stdout, stderr := record.output()
					entry.WithFields(logrus.Fields{"actual_stdout": responseText(stdout), "actual_stderr": responseText(stderr)}).Info("replay Exec output")
				}
			}
		}
		responses.Calls = append(responses.Calls, core.ReplayCallResponse{SourceSequence: call.Sequence, Status: status, ExpectedSHA256: call.ResponseSHA256, ActualSHA256: record.SHA256})
	}
	if len(records) != count {
		responses.Status, responses.Reason = "unavailable", "replay bridge audit has missing or unknown calls"
	}
	if responses.Status != "available" {
		responses.Calls = nil
	}
	return responses
}

func harborResponseStatus(call Call, record auditResponse) string {
	switch record.GRPCCode {
	case "":
		return ""
	case "OK":
	default:
		return "rpc_failed"
	}
	switch record.Status {
	case "completed":
	case "failed", "canceled", "rejected":
		return "operation_failed"
	default:
		return ""
	}
	if record.ReplayError == "response-send-failed" {
		return "operation_failed"
	}
	switch call.Operation {
	case "Exec":
		if call.HarborOutput == nil || record.ExitCode == nil {
			return ""
		}
		if *record.ExitCode != call.StrictExitCode() {
			return "exit_mismatch"
		}
		if call.HarborTruncated || record.Truncated || !record.Debug {
			return "inconclusive"
		}
		stdout, stderr := record.output()
		expected := *call.HarborOutput
		if len(expected) != len(stdout)+len(stderr) {
			return "length_mismatch"
		}
		if len(stdout) != 0 && len(stderr) != 0 {
			return "length_matched"
		}
		actual := stdout
		if len(stdout) == 0 {
			actual = stderr
		}
		if !bytes.Equal([]byte(expected), actual) {
			return "drifted"
		}
		return "matched"
	case "WriteFile":
		// A successful bridge write replies with the size of the accepted body.
		if record.StdinBytes == nil || record.ReplayError != "" && record.ReplayError != "write-content-omitted" {
			return ""
		}
		if *record.StdinBytes != int64(len(call.Body)) {
			return "length_mismatch"
		}
		return "matched"
	}
	return ""
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
