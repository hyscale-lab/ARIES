package terminalbench

import (
	"strings"
	"testing"
)

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

func TestValidateFixGitCTRFAcceptsPinnedResult(t *testing.T) {
	if err := validateCTRF(strings.NewReader(validFixGitCTRF)); err != nil {
		t.Fatalf("validateCTRF() error = %v", err)
	}
}

func TestValidateFixGitCTRFRejectsInvalidPinnedResult(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"unknown field", strings.Replace(validFixGitCTRF, `"version": "8.4.1"`, `"version": "8.4.1", "future": true`, 1), "unknown field"},
		{"trailing value", validFixGitCTRF + `{}`, "multiple JSON values"},
		{"wrong tool", strings.Replace(validFixGitCTRF, `"name": "pytest"`, `"name": "other"`, 1), "unexpected CTRF tool"},
		{"missing summary field", strings.Replace(validFixGitCTRF, `"other": 0,`, ``, 1), "missing a required"},
		{"wrong total", strings.Replace(validFixGitCTRF, `"tests": 2,`, `"tests": 3,`, 1), "want exactly"},
		{"failure count", strings.Replace(validFixGitCTRF, `"failed": 0,`, `"failed": 1,`, 1), "want exactly"},
		{"skip count", strings.Replace(validFixGitCTRF, `"skipped": 0,`, `"skipped": 1,`, 1), "want exactly"},
		{"pending count", strings.Replace(validFixGitCTRF, `"pending": 0,`, `"pending": 1,`, 1), "want exactly"},
		{"other count", strings.Replace(validFixGitCTRF, `"other": 0,`, `"other": 1,`, 1), "want exactly"},
		{"failed test", strings.Replace(validFixGitCTRF, `"status": "passed"`, `"status": "failed"`, 1), "want passed"},
		{"duplicate test", strings.Replace(validFixGitCTRF, `test_layout_file`, `test_about_file`, 1), "duplicate"},
		{"unexpected test", strings.Replace(validFixGitCTRF, `test_layout_file`, `test_other_file`, 1), "unexpected"},
		{"parameterized test", strings.Replace(validFixGitCTRF, `test_about_file"`, `test_about_file[case]"`, 1), "invalid"},
		{"wrong source", strings.Replace(validFixGitCTRF, `test_outputs.py::test_about_file`, `other.py::test_about_file`, 1), "invalid"},
		{"missing retry", strings.Replace(validFixGitCTRF, `        "retries": 0,
`, ``, 1), "retry count"},
		{"wrong file", strings.Replace(validFixGitCTRF, `../../tests/test_outputs.py`, `../../tests/other.py`, 1), "unexpected file path"},
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
