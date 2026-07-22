package terminalbench

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validateCTRF(reader io.Reader) error {
	_, err := readCTRF(reader)
	return err
}

func TestValidateCTRFFileRejectsRewardOneWithoutTests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctrf.json")
	content := `{"results":{"summary":{"tests":0,"passed":0,"failed":0,"skipped":0,"pending":0,"other":0},"tests":[]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCTRFFile(path, 1); err == nil || !strings.Contains(err.Error(), "reward is 1") {
		t.Fatalf("validateCTRFFile() error = %v, want reward-1 proof failure", err)
	}
}

const validFixGitCTRF = `{
  "results": {
    "tool": {"name": "pytest", "version": "8.4.1"},
    "summary": {
      "tests": 2,
      "passed": 2,
      "failed": 0,
      "skipped": 0,
      "pending": 0,
      "other": 0,
      "start": 1720000000.0,
      "stop": 1720000001.0
    },
    "tests": [
      {
        "name": "test_outputs.py::test_about_file",
        "status": "passed",
		"raw_status": "passed",
        "duration": 0.1,
        "start": 1720000000.0,
        "stop": 1720000000.5,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      },
      {
        "name": "test_outputs.py::test_layout_file",
        "status": "passed",
        "duration": 0.1,
        "start": 1720000000.5,
        "stop": 1720000001.0,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      }
    ]
  }
}`

const validParameterizedCTRF = `{
  "results": {
    "tool": {"name": "pytest", "version": "8.4.1"},
    "summary": {
      "tests": 3,
      "passed": 3,
      "failed": 0,
      "skipped": 0,
      "pending": 0,
      "other": 0,
      "start": 1720000000.0,
      "stop": 1720000002.0
    },
    "tests": [
      {
        "name": "test_outputs.py::test_arbitrary[case-a]",
        "status": "passed",
        "duration": 0.1,
        "start": 1720000000.0,
        "stop": 1720000000.5,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      },
      {
        "name": "test_outputs.py::test_arbitrary[case-b]",
        "status": "passed",
        "duration": 0.1,
        "start": 1720000000.5,
        "stop": 1720000001.0,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      },
      {
        "name": "test_outputs.py::test_unrelated_name",
        "status": "passed",
        "duration": 0.1,
        "start": 1720000001.0,
        "stop": 1720000002.0,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      }
    ]
  }
}`

const validRewardZeroCTRF = `{
  "results": {
    "tool": {"name": "pytest", "version": "8.4.1"},
    "summary": {
      "tests": 2,
      "passed": 1,
      "failed": 1,
      "skipped": 0,
      "pending": 0,
      "other": 0,
      "start": 1720000000.0,
      "stop": 1720000001.0
    },
    "tests": [
      {
        "name": "test_outputs.py::test_passes",
        "status": "passed",
        "duration": 0.1,
        "start": 1720000000.0,
        "stop": 1720000000.5,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      },
      {
        "name": "test_outputs.py::test_legitimate_failure",
        "status": "failed",
        "duration": 0.1,
        "start": 1720000000.5,
        "stop": 1720000001.0,
        "retries": 0,
        "file_path": "../../tests/test_outputs.py"
      }
    ]
  }
}`

func TestValidateFixGitCTRFAcceptsPinnedResult(t *testing.T) {
	if err := validateCTRF(strings.NewReader(validFixGitCTRF)); err != nil {
		t.Fatalf("validateCTRF() error = %v", err)
	}
}

func TestValidateGenericCTRFAcceptsArbitraryParameterizedTestSet(t *testing.T) {
	for _, report := range []string{
		validParameterizedCTRF,
		validRewardZeroCTRF,
		strings.Replace(validFixGitCTRF, `"version": "8.4.1"`, `"version": "future", "future": true`, 1),
		strings.Replace(validFixGitCTRF, `        "retries": 0,
`, ``, 1),
	} {
		if err := validateCTRF(strings.NewReader(report)); err != nil {
			t.Fatalf("validateCTRF() rejected a consistent generic report: %v", err)
		}
	}
}

func TestValidateGenericCTRFRejectsInconsistentResult(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"trailing value", validFixGitCTRF + `{}`, "multiple JSON values"},
		{"missing summary field", strings.Replace(validFixGitCTRF, `"other": 0,`, ``, 1), "missing a required count"},
		{"record total mismatch", strings.Replace(validFixGitCTRF, `"tests": 2,`, `"tests": 3,`, 1), "contains 2 test records"},
		{"summary counts mismatch", strings.Replace(validFixGitCTRF, `"failed": 0,`, `"failed": 1,`, 1), "do not add up"},
		{"record status mismatch", strings.Replace(validFixGitCTRF, `"status": "passed"`, `"status": "failed"`, 1), "records contain"},
		{"unsupported status", strings.Replace(validFixGitCTRF, `"status": "passed"`, `"status": "unknown"`, 1), "unsupported status"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCTRF(strings.NewReader(test.content))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateCTRF() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
