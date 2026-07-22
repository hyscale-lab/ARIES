package terminalbench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxCTRFBytes = 16 << 20

type ctrfReport struct {
	Results *ctrfResults `json:"results"`
}

type ctrfResults struct {
	Summary *ctrfSummary `json:"summary"`
	Tests   []ctrfTest   `json:"tests"`
}

type ctrfSummary struct {
	Tests   *int `json:"tests"`
	Passed  *int `json:"passed"`
	Failed  *int `json:"failed"`
	Skipped *int `json:"skipped"`
	Pending *int `json:"pending"`
	Other   *int `json:"other"`
}

type ctrfTest struct {
	Status string `json:"status"`
}

func validateCTRFFile(filePath string, reward float64) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open verifier CTRF: %w", err)
	}
	defer file.Close()
	summary, err := readCTRF(file)
	if err != nil {
		return fmt.Errorf("validate verifier CTRF: %w", err)
	}
	if reward == 1 && (*summary.Tests == 0 || *summary.Passed != *summary.Tests) {
		return fmt.Errorf("validate verifier CTRF: reward is 1 but only %d of %d tests passed", *summary.Passed, *summary.Tests)
	}
	return nil
}

func readCTRF(reader io.Reader) (ctrfSummary, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maxCTRFBytes+1))
	if err != nil {
		return ctrfSummary{}, fmt.Errorf("read CTRF: %w", err)
	}
	if len(content) > maxCTRFBytes {
		return ctrfSummary{}, fmt.Errorf("CTRF exceeds %d bytes", maxCTRFBytes)
	}
	return validateCTRFContent(content)
}

func validateCTRFContent(content []byte) (ctrfSummary, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	var report ctrfReport
	if err := decoder.Decode(&report); err != nil {
		return ctrfSummary{}, fmt.Errorf("decode CTRF output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ctrfSummary{}, errors.New("CTRF output contains multiple JSON values")
		}
		return ctrfSummary{}, fmt.Errorf("read trailing CTRF output: %w", err)
	}
	if report.Results == nil || report.Results.Summary == nil {
		return ctrfSummary{}, errors.New("CTRF output is missing results.summary")
	}
	if err := validateCTRFSummary(*report.Results.Summary, report.Results.Tests); err != nil {
		return ctrfSummary{}, err
	}
	return *report.Results.Summary, nil
}

func validateCTRFSummary(summary ctrfSummary, tests []ctrfTest) error {
	counts := []*int{summary.Tests, summary.Passed, summary.Failed, summary.Skipped, summary.Pending, summary.Other}
	for _, count := range counts {
		if count == nil {
			return errors.New("CTRF summary is missing a required count")
		}
		if *count < 0 {
			return errors.New("CTRF summary contains a negative count")
		}
	}
	if *summary.Tests != len(tests) {
		return fmt.Errorf("CTRF summary reports %d tests but contains %d test records", *summary.Tests, len(tests))
	}
	if *summary.Passed+*summary.Failed+*summary.Skipped+*summary.Pending+*summary.Other != *summary.Tests {
		return errors.New("CTRF summary status counts do not add up to tests")
	}

	actual := map[string]int{}
	for index, test := range tests {
		switch test.Status {
		case "passed", "failed", "skipped", "pending", "other":
			actual[test.Status]++
		default:
			return fmt.Errorf("CTRF test record %d has unsupported status %q", index, test.Status)
		}
	}
	expected := map[string]int{
		"passed": *summary.Passed, "failed": *summary.Failed, "skipped": *summary.Skipped,
		"pending": *summary.Pending, "other": *summary.Other,
	}
	for status, count := range expected {
		if actual[status] != count {
			return fmt.Errorf("CTRF summary reports %d %s tests but records contain %d", count, status, actual[status])
		}
	}
	return nil
}
