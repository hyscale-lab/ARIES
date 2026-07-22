package terminalbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const maxCTRFBytes = 1 << 20

var fixGitTests = map[string]struct{}{
	"test_about_file":  {},
	"test_layout_file": {},
}

type ctrfReport struct {
	Results ctrfResults `json:"results"`
}

type ctrfResults struct {
	Tool    ctrfTool    `json:"tool"`
	Summary ctrfSummary `json:"summary"`
	Tests   []ctrfTest  `json:"tests"`
}

type ctrfTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ctrfSummary struct {
	Tests   *int     `json:"tests"`
	Passed  *int     `json:"passed"`
	Failed  *int     `json:"failed"`
	Skipped *int     `json:"skipped"`
	Pending *int     `json:"pending"`
	Other   *int     `json:"other"`
	Start   *float64 `json:"start"`
	Stop    *float64 `json:"stop"`
}

type ctrfTest struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	RawStatus string   `json:"raw_status"`
	Trace     string   `json:"trace"`
	Message   string   `json:"message"`
	Duration  *float64 `json:"duration"`
	Start     *float64 `json:"start"`
	Stop      *float64 `json:"stop"`
	Retries   *int     `json:"retries"`
	FilePath  string   `json:"file_path"`
}

func validateCTRFFile(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open verifier CTRF: %w", err)
	}
	defer file.Close()

	limited := io.LimitReader(file, maxCTRFBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read verifier CTRF: %w", err)
	}
	if len(content) > maxCTRFBytes {
		return fmt.Errorf("verifier CTRF exceeds %d bytes", maxCTRFBytes)
	}
	if err := validateCTRF(strings.NewReader(string(content))); err != nil {
		return fmt.Errorf("validate verifier CTRF: %w", err)
	}
	return nil
}

func validateCTRF(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	var report ctrfReport
	if err := decoder.Decode(&report); err != nil {
		return fmt.Errorf("decode pinned pytest-json-ctrf output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("pinned pytest-json-ctrf output contains multiple JSON values")
		}
		return fmt.Errorf("read trailing pinned pytest-json-ctrf output: %w", err)
	}

	if report.Results.Tool.Name != "pytest" || report.Results.Tool.Version != "8.4.1" {
		return fmt.Errorf("unexpected CTRF tool %q version %q; want pytest 8.4.1", report.Results.Tool.Name, report.Results.Tool.Version)
	}
	if err := validateCTRFSummary(report.Results.Summary); err != nil {
		return err
	}
	if len(report.Results.Tests) != len(fixGitTests) {
		return fmt.Errorf("CTRF contains %d test records; want exactly %d", len(report.Results.Tests), len(fixGitTests))
	}

	seen := make(map[string]struct{}, len(report.Results.Tests))
	for _, test := range report.Results.Tests {
		name, err := fixGitTestName(test.Name)
		if err != nil {
			return err
		}
		if _, ok := fixGitTests[name]; !ok {
			return fmt.Errorf("CTRF contains unexpected fix-git test %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("CTRF contains duplicate fix-git test %q", name)
		}
		seen[name] = struct{}{}
		if test.Status != "passed" {
			return fmt.Errorf("CTRF test %q has status %q; want passed", name, test.Status)
		}
		if test.Duration == nil || *test.Duration < 0 || test.Start == nil || *test.Start <= 0 || test.Stop == nil || *test.Stop < *test.Start {
			return fmt.Errorf("CTRF test %q has invalid timing fields", name)
		}
		if test.Retries == nil || *test.Retries != 0 {
			return fmt.Errorf("CTRF test %q has invalid retry count", name)
		}
		if path.Base(test.FilePath) != "test_outputs.py" {
			return fmt.Errorf("CTRF test %q has unexpected file path %q", name, test.FilePath)
		}
	}
	for name := range fixGitTests {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("CTRF is missing fix-git test %q", name)
		}
	}
	return nil
}

func validateCTRFSummary(summary ctrfSummary) error {
	if summary.Tests == nil || summary.Passed == nil || summary.Failed == nil || summary.Skipped == nil || summary.Pending == nil || summary.Other == nil || summary.Start == nil || summary.Stop == nil {
		return errors.New("CTRF summary is missing a required pinned field")
	}
	if *summary.Tests != 2 || *summary.Passed != 2 || *summary.Failed != 0 || *summary.Skipped != 0 || *summary.Pending != 0 || *summary.Other != 0 {
		return fmt.Errorf(
			"CTRF summary is tests=%d passed=%d failed=%d skipped=%d pending=%d other=%d; want exactly 2/2/0/0/0/0",
			*summary.Tests, *summary.Passed, *summary.Failed, *summary.Skipped, *summary.Pending, *summary.Other,
		)
	}
	if *summary.Start <= 0 || *summary.Stop < *summary.Start {
		return errors.New("CTRF summary has invalid start or stop time")
	}
	return nil
}

func fixGitTestName(nodeID string) (string, error) {
	parts := strings.Split(nodeID, "::")
	if len(parts) != 2 || path.Base(parts[0]) != "test_outputs.py" || parts[1] == "" || strings.Contains(parts[1], "[") {
		return "", fmt.Errorf("invalid fix-git CTRF test node ID %q", nodeID)
	}
	return parts[1], nil
}
